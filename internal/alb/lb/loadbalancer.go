package lb

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"

	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/alb/sg"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/alb/tags"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/albctx"

	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/k8s"

	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/aws/albwafregional"

	extensions "k8s.io/api/extensions/v1beta1"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/alb/ls"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/alb/tg"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/aws/albec2"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/aws/albelbv2"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/ingress/controller/store"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/pkg/util/log"
	util "github.com/kubernetes-sigs/aws-alb-ingress-controller/pkg/util/types"
	api "k8s.io/api/core/v1"
)

type NewDesiredLoadBalancerOptions struct {
	ExistingLoadBalancer *LoadBalancer
	Store                store.Storer
	Ingress              *extensions.Ingress
	CommonTags           *tags.Tags
}

// NewDesiredLoadBalancer returns a new loadbalancer.LoadBalancer based on the opts provided.
func NewDesiredLoadBalancer(o *NewDesiredLoadBalancerOptions) (newLoadBalancer *LoadBalancer, err error) {
	name := createLBName(o.Ingress.Namespace, o.Ingress.Name, o.Store.GetConfig().ALBNamePrefix)

	lbTags := o.CommonTags.Copy()

	vpc, err := albec2.EC2svc.GetVPCID()
	if err != nil {
		return nil, err
	}

	annos, err := o.Store.GetIngressAnnotations(k8s.MetaNamespaceKey(o.Ingress))
	if err != nil {
		return nil, err
	}

	newLoadBalancer = &LoadBalancer{
		id:   name,
		tags: lbTags,
		options: options{
			desired: opts{
				webACLId: annos.LoadBalancer.WebACLId,
			},
		},
		lb: lb{
			desired: &elbv2.LoadBalancer{
				AvailabilityZones: annos.LoadBalancer.Subnets.AsAvailabilityZones(),
				LoadBalancerName:  aws.String(name),
				Scheme:            annos.LoadBalancer.Scheme,
				IpAddressType:     annos.LoadBalancer.IPAddressType,
				VpcId:             vpc,
			},
		},
	}

	var existingtgs tg.TargetGroups
	var existingls ls.Listeners
	existinglb := o.ExistingLoadBalancer

	if existinglb != nil {
		// we had an existing LoadBalancer in ingress, so just copy the desired state over
		existinglb.lb.desired = newLoadBalancer.lb.desired
		existinglb.tags = newLoadBalancer.tags
		existinglb.options.desired.webACLId = newLoadBalancer.options.desired.webACLId

		newLoadBalancer = existinglb
		existingtgs = existinglb.targetgroups
		existingls = existinglb.listeners
	}

	tgTags := o.CommonTags.Copy()

	// Assemble the target groups
	newLoadBalancer.targetgroups, err = tg.NewDesiredTargetGroups(&tg.NewDesiredTargetGroupsOptions{
		Ingress:              o.Ingress,
		LoadBalancerID:       newLoadBalancer.id,
		ExistingTargetGroups: existingtgs,
		Store:                o.Store,
		CommonTags:           tgTags,
	})

	if err != nil {
		return newLoadBalancer, err
	}

	// Assemble the listeners
	newLoadBalancer.listeners, err = ls.NewDesiredListeners(&ls.NewDesiredListenersOptions{
		Ingress:           o.Ingress,
		Store:             o.Store,
		ExistingListeners: existingls,
		TargetGroups:      newLoadBalancer.targetgroups,
	})

	// Assemble SecurityGroups
	lbPorts := []int64{}
	for _, port := range annos.LoadBalancer.Ports {
		lbPorts = append(lbPorts, port.Port)
	}
	newLoadBalancer.sgAssociation = sg.Association{
		LbID:           name,
		LbPorts:        lbPorts,
		LbInboundCIDRs: annos.LoadBalancer.InboundCidrs,
		ExternalSGIDs:  aws.StringValueSlice(annos.LoadBalancer.SecurityGroups),
	}

	// Assemble Attributes
	newLoadBalancer.attributes, err = NewAttributes(annos.LoadBalancer.Attributes)
	if err != nil {
		return newLoadBalancer, err
	}

	return newLoadBalancer, err
}

type NewCurrentLoadBalancerOptions struct {
	LoadBalancer *elbv2.LoadBalancer
	TargetGroups map[string][]*elbv2.TargetGroup
}

// NewCurrentLoadBalancer returns a new loadbalancer.LoadBalancer based on an elbv2.LoadBalancer.
func NewCurrentLoadBalancer(o *NewCurrentLoadBalancerOptions) (newLoadBalancer *LoadBalancer, err error) {
	// Check WAF
	webACLResult, err := albwafregional.WAFRegionalsvc.GetWebACLSummary(o.LoadBalancer.LoadBalancerArn)
	if err != nil {
		return newLoadBalancer, fmt.Errorf("failed to get associated Web ACL: %s", err.Error())
	}
	var webACLId *string
	if webACLResult != nil {
		webACLId = webACLResult.WebACLId
	}

	newLoadBalancer = &LoadBalancer{
		id:            *o.LoadBalancer.LoadBalancerName,
		tags:          &tags.Tags{},
		lb:            lb{current: o.LoadBalancer},
		attributes:    &Attributes{},
		sgAssociation: sg.Association{LbID: *o.LoadBalancer.LoadBalancerName},
		options: options{current: opts{
			webACLId: webACLId,
		}},
	}

	// Assemble target groups
	targetGroups := o.TargetGroups[*o.LoadBalancer.LoadBalancerArn]

	newLoadBalancer.targetgroups, err = tg.NewCurrentTargetGroups(&tg.NewCurrentTargetGroupsOptions{
		TargetGroups:   targetGroups,
		LoadBalancerID: newLoadBalancer.id,
	})
	if err != nil {
		return newLoadBalancer, err
	}

	// Assemble listeners
	listeners, err := albelbv2.ELBV2svc.DescribeListenersForLoadBalancer(o.LoadBalancer.LoadBalancerArn)
	if err != nil {
		return newLoadBalancer, err
	}

	newLoadBalancer.listeners, err = ls.NewCurrentListeners(&ls.NewCurrentListenersOptions{
		TargetGroups: newLoadBalancer.targetgroups,
		Listeners:    listeners,
	})
	if err != nil {
		return newLoadBalancer, err
	}

	return newLoadBalancer, err
}

// Reconcile compares the current and desired state of this LoadBalancer instance. Comparison
// results in no action, the creation, the deletion, or the modification of an AWS ELBV2 to
// satisfy the ingress's current state.
func (l *LoadBalancer) Reconcile(ctx context.Context, rOpts *ReconcileOptions) []error {
	var errors []error
	lbc := l.lb.current
	lbd := l.lb.desired

	switch {
	case lbd == nil: // lb should be deleted
		if lbc == nil {
			break
		}
		albctx.GetLogger(ctx).Infof("Start ELBV2 deletion.")
		if err := l.delete(ctx, rOpts); err != nil {
			errors = append(errors, err)
			break
		}
		albctx.GetEventf(ctx)(api.EventTypeNormal, "DELETE", "%s deleted", *lbc.LoadBalancerName)
		albctx.GetLogger(ctx).Infof("Completed ELBV2 deletion. Name: %s | ARN: %s",
			*lbc.LoadBalancerName,
			*lbc.LoadBalancerArn)

	case lbc == nil: // lb doesn't exist and should be created
		albctx.GetLogger(ctx).Infof("Start ELBV2 creation.")
		if err := l.create(ctx, rOpts); err != nil {
			errors = append(errors, err)
			return errors
		}
		lbc = l.lb.current
		albctx.GetEventf(ctx)(api.EventTypeNormal, "CREATE", "%s created", *lbc.LoadBalancerName)
		albctx.GetLogger(ctx).Infof("Completed ELBV2 creation. Name: %s | ARN: %s",
			*lbc.LoadBalancerName,
			*lbc.LoadBalancerArn)

	default: // check for diff between lb current and desired, modify if necessary
		if err := l.modify(ctx, rOpts); err != nil {
			errors = append(errors, err)
			break
		}
	}

	tgsOpts := &tg.ReconcileOptions{
		Store:                  rOpts.Store,
		TgAttributesController: rOpts.TgAttributesController,
		TgTargetsController:    rOpts.TgTargetsController,
		TagsController:         rOpts.TagsController,
		IgnoreDeletes:          true,
	}

	// Creates target groups
	tgs, err := l.targetgroups.Reconcile(ctx, tgsOpts)
	if err != nil {
		errors = append(errors, err)
	} else {
		l.targetgroups = tgs
	}

	lsOpts := &ls.ReconcileOptions{
		LoadBalancerArn: lbc.LoadBalancerArn,
		TargetGroups:    l.targetgroups,
		Ingress:         rOpts.Ingress,
		RulesController: rOpts.RulesController,
		Store:           rOpts.Store,
	}
	if ltnrs, err := l.listeners.Reconcile(ctx, lsOpts); err != nil {
		errors = append(errors, err)
	} else {
		l.listeners = ltnrs
	}

	// TODO: currently this works fine since every listener get same actions,
	// when this precondition don't hold, we need to consider deletion at cross-listener level.

	// // Does not consider TG used for listener default action
	// for _, listener := range l.listeners {
	// 	unusedTGs := listener.UnusedTargetGroups(l.targetgroups)
	// 	unusedTGs.StripDesiredState()
	// }

	// removes target groups
	tgsOpts.IgnoreDeletes = false
	tgs, err = l.targetgroups.Reconcile(ctx, tgsOpts)
	if err != nil {
		errors = append(errors, err)
	} else {
		l.targetgroups = tgs
	}

	if !l.deleted {
		l.tags.Arn = aws.StringValue(l.lb.current.LoadBalancerArn)
		err := rOpts.TagsController.Reconcile(ctx, l.tags)
		if err != nil {
			errors = append(errors, fmt.Errorf("failed tagging due to %s", err.Error()))
		}

		l.sgAssociation.LbArn = aws.StringValue(l.lb.current.LoadBalancerArn)
		l.sgAssociation.Targets = l.targetgroups
		err = rOpts.SgAssociationController.Reconcile(ctx, &l.sgAssociation)
		if err != nil {
			errors = append(errors, fmt.Errorf("failed association of SecurityGroups due to %s", err.Error()))
		}

		l.attributes.LbArn = aws.StringValue(l.lb.current.LoadBalancerArn)
		err = rOpts.LbAttributesController.Reconcile(ctx, l.attributes)
		if err != nil {
			errors = append(errors, fmt.Errorf("failed configuration of load balancer attributes due to %s", err.Error()))
		}
	}

	return errors
}

// create requests a new ELBV2 is created in AWS.
func (l *LoadBalancer) create(ctx context.Context, rOpts *ReconcileOptions) error {
	desired := l.lb.desired
	in := &elbv2.CreateLoadBalancerInput{
		Name:          desired.LoadBalancerName,
		Subnets:       util.AvailabilityZones(desired.AvailabilityZones).AsSubnets(),
		Scheme:        desired.Scheme,
		IpAddressType: desired.IpAddressType,
		Tags:          l.tags.AsELBV2(),
	}

	o, err := albelbv2.ELBV2svc.CreateLoadBalancer(in)
	if err != nil {
		albctx.GetEventf(ctx)(api.EventTypeWarning, "ERROR", "Error creating %s: %s", *in.Name, err.Error())
		albctx.GetLogger(ctx).Errorf("Failed to create ELBV2: %s", err.Error())
		return err
	}

	// lb created. set to current
	l.lb.current = o.LoadBalancers[0]

	if l.options.desired.webACLId != nil {
		_, err = albwafregional.WAFRegionalsvc.Associate(l.lb.current.LoadBalancerArn, l.options.desired.webACLId)
		if err != nil {
			albctx.GetEventf(ctx)(api.EventTypeWarning, "ERROR", "%s Web ACL (%s) association failed: %s", *l.lb.current.LoadBalancerName, l.options.desired.webACLId, err.Error())
			albctx.GetLogger(ctx).Errorf("Failed setting Web ACL (%s) association: %s", l.options.desired.webACLId, err.Error())
			return err
		}
	}
	return nil
}

// modify modifies the attributes of an existing ALB in AWS.
func (l *LoadBalancer) modify(ctx context.Context, rOpts *ReconcileOptions) error {
	needsMod, canMod := l.needsModification(ctx)
	if needsMod == 0 {
		return nil
	}

	if canMod {
		// Modify Subnets
		if needsMod&subnetsModified != 0 {
			albctx.GetLogger(ctx).Infof("Modifying ELBV2 subnets to %v.", log.Prettify(l.lb.current.AvailabilityZones))
			if _, err := albelbv2.ELBV2svc.SetSubnets(&elbv2.SetSubnetsInput{
				LoadBalancerArn: l.lb.current.LoadBalancerArn,
				Subnets:         util.AvailabilityZones(l.lb.desired.AvailabilityZones).AsSubnets(),
			}); err != nil {
				albctx.GetEventf(ctx)(api.EventTypeWarning, "ERROR", "%s subnet modification failed: %s", *l.lb.current.LoadBalancerName, err.Error())
				return fmt.Errorf("Failed setting ELBV2 subnets: %s", err)
			}
			l.lb.current.AvailabilityZones = l.lb.desired.AvailabilityZones
			albctx.GetEventf(ctx)(api.EventTypeNormal, "MODIFY", "%s subnets modified", *l.lb.current.LoadBalancerName)
		}

		// Modify IP address type
		if needsMod&ipAddressTypeModified != 0 {
			albctx.GetLogger(ctx).Infof("Modifying IP address type modification to %v.", *l.lb.current.IpAddressType)
			if _, err := albelbv2.ELBV2svc.SetIpAddressType(&elbv2.SetIpAddressTypeInput{
				LoadBalancerArn: l.lb.current.LoadBalancerArn,
				IpAddressType:   l.lb.desired.IpAddressType,
			}); err != nil {
				albctx.GetEventf(ctx)(api.EventTypeNormal, "ERROR", "%s ip address type modification failed: %s", *l.lb.current.LoadBalancerName, err.Error())
				return fmt.Errorf("Failed setting ELBV2 IP address type: %s", err)
			}
			l.lb.current.IpAddressType = l.lb.desired.IpAddressType
			albctx.GetEventf(ctx)(api.EventTypeNormal, "MODIFY", "%s ip address type modified", *l.lb.current.LoadBalancerName)
		}

		// Modify Web ACL
		if needsMod&webACLAssociationModified != 0 {
			if l.options.desired.webACLId != nil { // Associate
				albctx.GetLogger(ctx).Infof("Associating %v Web ACL.", *l.options.desired.webACLId)
				if _, err := albwafregional.WAFRegionalsvc.Associate(l.lb.current.LoadBalancerArn, l.options.desired.webACLId); err != nil {
					albctx.GetEventf(ctx)(api.EventTypeWarning, "ERROR", "%s Web ACL (%s) association failed: %s", *l.lb.current.LoadBalancerName, *l.options.desired.webACLId, err.Error())
					return fmt.Errorf("Failed associating Web ACL: %s", err.Error())
				}
				l.options.current.webACLId = l.options.desired.webACLId
				albctx.GetEventf(ctx)(api.EventTypeNormal, "MODIFY", "Web ACL association updated to %s", *l.options.desired.webACLId)
			} else { // Disassociate
				albctx.GetLogger(ctx).Infof("Disassociating Web ACL.")
				if _, err := albwafregional.WAFRegionalsvc.Disassociate(l.lb.current.LoadBalancerArn); err != nil {
					albctx.GetEventf(ctx)(api.EventTypeWarning, "ERROR", "%s Web ACL disassociation failed: %s", *l.lb.current.LoadBalancerName, err.Error())
					return fmt.Errorf("Failed removing Web ACL association: %s", err.Error())
				}
				l.options.current.webACLId = l.options.desired.webACLId
				albctx.GetEventf(ctx)(api.EventTypeNormal, "MODIFY", "Web ACL disassociated")
			}
		}

	} else {
		// Modification is needed, but required full replacement of ALB.
		// TODO improve this process, it generally fails some deletions and completes in the next sync
		albctx.GetLogger(ctx).Infof("Start ELBV2 full modification (delete and create).")
		albctx.GetEventf(ctx)(api.EventTypeNormal, "REBUILD", "Impossible modification requested, rebuilding %s", *l.lb.current.LoadBalancerName)
		l.delete(ctx, rOpts)
		// Since listeners and rules are deleted during lb deletion, ensure their current state is removed
		// as they'll no longer exist.
		l.listeners.StripCurrentState()
		l.create(ctx, rOpts)
		albctx.GetLogger(ctx).Infof("Completed ELBV2 full modification (delete and create). Name: %s | ARN: %s",
			*l.lb.current.LoadBalancerName, *l.lb.current.LoadBalancerArn)

	}

	return nil
}

// delete Deletes the load balancer from AWS.
func (l *LoadBalancer) delete(ctx context.Context, rOpts *ReconcileOptions) error {
	l.deleted = true

	l.sgAssociation.LbArn = aws.StringValue(l.lb.current.LoadBalancerArn)
	err := rOpts.SgAssociationController.Delete(ctx, &l.sgAssociation)
	if err != nil {
		return fmt.Errorf("failed disassociation of SecurityGroups due to %s", err.Error())
	}

	// we need to disassociate the WAF before deletion
	if l.options.current.webACLId != nil {
		if _, err := albwafregional.WAFRegionalsvc.Disassociate(l.lb.current.LoadBalancerArn); err != nil {
			albctx.GetEventf(ctx)(api.EventTypeWarning, "ERROR", "Error disassociating Web ACL for %s: %s", *l.lb.current.LoadBalancerName, err.Error())
			return fmt.Errorf("Failed disassociation of ELBV2 Web ACL: %s.", err.Error())
		}
	}

	in := &elbv2.DeleteLoadBalancerInput{
		LoadBalancerArn: l.lb.current.LoadBalancerArn,
	}

	if _, err = albelbv2.ELBV2svc.DeleteLoadBalancer(in); err != nil {
		albctx.GetEventf(ctx)(api.EventTypeWarning, "ERROR", "Error deleting %s: %s", *l.lb.current.LoadBalancerName, err.Error())
		return fmt.Errorf("Failed deletion of ELBV2: %s.", err.Error())
	}
	return nil
}

// needsModification returns if a LB needs to be modified and if it can be modified in place
// first parameter is true if the LB needs to be changed
// second parameter true if it can be changed in place
func (l *LoadBalancer) needsModification(ctx context.Context) (loadBalancerChange, bool) {
	var changes loadBalancerChange

	clb := l.lb.current
	dlb := l.lb.desired
	copts := l.options.current
	dopts := l.options.desired

	// In the case that the LB does not exist yet
	if clb == nil {
		albctx.GetLogger(ctx).Debugf("Current Load Balancer is undefined")
		return changes, true
	}

	if !util.DeepEqual(clb.Scheme, dlb.Scheme) {
		albctx.GetLogger(ctx).Debugf("Scheme needs to be changed (%v != %v)", log.Prettify(clb.Scheme), log.Prettify(dlb.Scheme))
		changes |= schemeModified
		return changes, false
	}

	if !util.DeepEqual(clb.IpAddressType, dlb.IpAddressType) {
		albctx.GetLogger(ctx).Debugf("IpAddressType needs to be changed (%v != %v)", log.Prettify(clb.IpAddressType), log.Prettify(dlb.IpAddressType))
		changes |= ipAddressTypeModified
	}

	currentSubnets := util.AvailabilityZones(clb.AvailabilityZones).AsSubnets()
	desiredSubnets := util.AvailabilityZones(dlb.AvailabilityZones).AsSubnets()
	sort.Sort(currentSubnets)
	sort.Sort(desiredSubnets)
	if log.Prettify(currentSubnets) != log.Prettify(desiredSubnets) {
		albctx.GetLogger(ctx).Debugf("AvailabilityZones needs to be changed (%v != %v)", log.Prettify(currentSubnets), log.Prettify(desiredSubnets))
		changes |= subnetsModified
	}

	if c := l.options.needsModification(); c != 0 {
		changes |= c
		if changes&webACLAssociationModified != 0 {
			albctx.GetLogger(ctx).Debugf("WAF needs to be changed: (%v != %v)", log.Prettify(copts.webACLId), log.Prettify(dopts.webACLId))
		}
	}
	return changes, true
}

// StripDesiredState removes the DesiredLoadBalancer from the LoadBalancer
func (l *LoadBalancer) StripDesiredState() {
	l.lb.desired = nil
	l.options.desired.webACLId = nil
	if l.listeners != nil {
		l.listeners.StripDesiredState()
	}
	if l.targetgroups != nil {
		l.targetgroups.StripDesiredState()
	}
}

func createLBName(namespace string, ingressName string, clustername string) string {
	hasher := md5.New()
	hasher.Write([]byte(namespace + ingressName))
	hash := hex.EncodeToString(hasher.Sum(nil))[:4]

	r, _ := regexp.Compile("[[:^alnum:]]")
	name := fmt.Sprintf("%s-%s-%s",
		r.ReplaceAllString(clustername, "-"),
		r.ReplaceAllString(namespace, ""),
		r.ReplaceAllString(ingressName, ""),
	)
	if len(name) > 26 {
		name = name[:26]
	}
	name = name + "-" + hash
	return name
}

// Hostname returns the AWS hostname of the load balancer
func (l *LoadBalancer) Hostname() *string {
	if l.lb.current == nil {
		return nil
	}
	return l.lb.current.DNSName
}
