package alb

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/coreos/alb-ingress-controller/awsutil"
	"github.com/coreos/alb-ingress-controller/controller/util"
	"github.com/coreos/alb-ingress-controller/log"
	api "k8s.io/api/core/v1"
	extensions "k8s.io/api/extensions/v1beta1"
)

// Rule contains a current/desired Rule
type Rule struct {
	IngressID   *string
	SvcName     string
	CurrentRule *elbv2.Rule
	DesiredRule *elbv2.Rule
	deleted     bool
}

// NewRule returns an alb.Rule based on the provided parameters.
func NewRule(path extensions.HTTPIngressPath, ingressID *string) *Rule {
	r := &elbv2.Rule{
		Actions: []*elbv2.Action{
			{
				// TargetGroupArn: targetGroupArn,
				Type: aws.String("forward"),
			},
		},
	}

	if path.Path == "/" || path.Path == "" {
		r.IsDefault = aws.Bool(true)
		r.Priority = aws.String("default")
	} else {
		r.IsDefault = aws.Bool(false)
		r.Conditions = []*elbv2.RuleCondition{
			{
				Field:  aws.String("path-pattern"),
				Values: []*string{&path.Path},
			},
		}
	}

	rule := &Rule{
		IngressID:   ingressID,
		SvcName:     path.Backend.ServiceName,
		DesiredRule: r,
	}
	return rule
}

// Reconcile compares the current and desired state of this Rule instance. Comparison
// results in no action, the creation, the deletion, or the modification of an AWS Rule to
// satisfy the ingress's current state.
func (r *Rule) Reconcile(rOpts *ReconcileOptions, l *Listener) error {
	switch {
	case r.DesiredRule == nil: // rule should be deleted
		if r.CurrentRule == nil {
			break
		}
		if *r.CurrentRule.IsDefault == true {
			break
		}
		log.Infof("Start Rule deletion.", *r.IngressID)
		if err := r.delete(rOpts); err != nil {
			return err
		}
		rOpts.Eventf(api.EventTypeNormal, "DELETE", "%s rule deleted", *r.CurrentRule.Priority)
		log.Infof("Completed Rule deletion. Rule: %s | Condition: %s", *r.IngressID,
			log.Prettify(r.CurrentRule.Conditions))

	case *r.DesiredRule.IsDefault: // rule is default (attached to listener), do nothing
		log.Debugf("Found desired rule that is a default and is already created with its respective listener. Rule: %s",
			*r.IngressID, log.Prettify(r.DesiredRule))
		r.CurrentRule = r.DesiredRule

	case r.CurrentRule == nil: // rule doesn't exist and should be created
		log.Infof("Start Rule creation.", *r.IngressID)
		if err := r.create(rOpts, l); err != nil {
			return err
		}
		rOpts.Eventf(api.EventTypeNormal, "CREATE", "%s rule created", *r.CurrentRule.Priority)
		log.Infof("Completed Rule creation. Rule: %s | Condition: %s", *r.IngressID,
			log.Prettify(r.CurrentRule.Conditions))

	case r.needsModification(): // diff between current and desired, modify rule
		log.Infof("Start Rule modification.", *r.IngressID)
		if err := r.modify(rOpts); err != nil {
			return err
		}
		rOpts.Eventf(api.EventTypeNormal, "MODIFY", "%s rule modified", *r.CurrentRule.Priority)
		log.Infof("Completed Rule modification. [UNIMPLEMENTED]", *r.IngressID)

	default:
		log.Debugf("No listener modification required.", *r.IngressID)
	}

	return nil
}

func (r *Rule) create(rOpts *ReconcileOptions, l *Listener) error {
	lb := rOpts.loadbalancer
	in := elbv2.CreateRuleInput{
		Actions:     r.DesiredRule.Actions,
		Conditions:  r.DesiredRule.Conditions,
		ListenerArn: l.CurrentListener.ListenerArn,
		Priority:    aws.Int64(lb.LastRulePriority),
	}

	in.Actions[0].TargetGroupArn = lb.TargetGroups[0].CurrentTargetGroup.TargetGroupArn
	tgIndex := lb.TargetGroups.LookupBySvc(r.SvcName)

	if tgIndex < 0 {
		log.Errorf("Failed to locate TargetGroup related to this service. Defaulting to first Target Group. SVC: %s", *r.IngressID, r.SvcName)
	} else {
		ctg := lb.TargetGroups[tgIndex].CurrentTargetGroup
		in.Actions[0].TargetGroupArn = ctg.TargetGroupArn
	}

	o, err := awsutil.ALBsvc.AddRule(in)
	if err != nil {
		rOpts.Eventf(api.EventTypeWarning, "ERROR", "Error creating %v rule: %s", *in.Priority, err.Error())
		log.Errorf("Failed Rule creation. Rule: %s | Error: %s", *r.IngressID,
			log.Prettify(r.DesiredRule), err.Error())
		return err
	}
	r.CurrentRule = o

	// Increase rule priority by 1 for each creation of a rule on this listener.
	// Note: All rules must have a unique priority.
	lb.LastRulePriority++
	return nil
}

func (r *Rule) modify(rOpts *ReconcileOptions) error {
	// TODO: Unimplemented
	return nil
}

func (r *Rule) delete(rOpts *ReconcileOptions) error {
	if r.CurrentRule == nil {
		log.Infof("Rule entered delete with no CurrentRule to delete. Rule: %s",
			*r.IngressID, log.Prettify(r))
		return nil
	}

	// If the current rule was a default, it's bound to the listener and won't be deleted from here.
	if *r.CurrentRule.IsDefault {
		log.Debugf("Deletion hit for default rule, which is bound to the Listener. It will not be deleted from here. Rule. Rule: %s",
			*r.IngressID, log.Prettify(r))
		return nil
	}

	in := elbv2.DeleteRuleInput{RuleArn: r.CurrentRule.RuleArn}
	if err := awsutil.ALBsvc.RemoveRule(in); err != nil {
		rOpts.Eventf(api.EventTypeWarning, "ERROR", "Error deleting %s rule: %s", *r.CurrentRule.Priority, err.Error())
		log.Infof("Failed Rule deletion. Error: %s", *r.IngressID, err.Error())
		return err
	}

	r.deleted = true
	return nil
}

func (r *Rule) needsModification() bool {
	cr := r.CurrentRule
	dr := r.DesiredRule

	switch {
	case cr == nil:
		return true
		// TODO: If we can populate the TargetGroupArn in NewALBIngressFromIngress, we can enable this
		// case awsutil.Prettify(cr.Actions) != awsutil.Prettify(dr.Actions):
		// 	return true
	case awsutil.Prettify(cr.Conditions) != awsutil.Prettify(dr.Conditions):
		return true
	}

	return false
}

// Equals returns true if the two CurrentRule and target rule are the same
// Does not compare priority, since this is not supported by the ingress spec
func (r *Rule) Equals(target *elbv2.Rule) bool {
	switch {
	case r.CurrentRule == nil && target == nil:
		return false
	case r.CurrentRule == nil && target != nil:
		return false
	case r.CurrentRule != nil && target == nil:
		return false
		// a rule is tightly wound to a listener which is also bound to a single TG
		// action only has 2 values, tg arn and a type, type is _always_ forward
		// case !util.DeepEqual(r.CurrentRule.Actions, target.Actions):
		// 	return false
	case !util.DeepEqual(r.CurrentRule.IsDefault, target.IsDefault):
		return false
	case !util.DeepEqual(r.CurrentRule.Conditions, target.Conditions):
		return false
	}
	return true
}
