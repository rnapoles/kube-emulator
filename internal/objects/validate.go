package objects

import (
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apimachineryvalidation "k8s.io/apimachinery/pkg/api/validation"
	"k8s.io/apimachinery/pkg/api/validation/path"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/labels"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"

	"github.com/rnapoles/kube-emulator/internal/registry"
)

// NameFuncFor picks the metadata.name validator the real API server uses.
func NameFuncFor(res *registry.Resource) apimachineryvalidation.ValidateNameFunc {
	switch {
	case res.Custom, res.Group == "rbac.authorization.k8s.io":
		return path.ValidatePathSegmentName
	case res.Key() == registry.KNamespaces:
		return apimachineryvalidation.NameIsDNSLabel
	case res.Key() == registry.KServices:
		return apimachineryvalidation.NameIsDNS1035Label
	default:
		return apimachineryvalidation.NameIsDNSSubdomain
	}
}

// ValidateMeta runs the real Kubernetes ObjectMeta validation.
func ValidateMeta(res *registry.Resource, u *U) field.ErrorList {
	return apimachineryvalidation.ValidateObjectMetaAccessor(u, res.Namespaced, NameFuncFor(res), field.NewPath("metadata"))
}

// ---------------------------------------------------------------- defaulting

func pullPolicyFor(image string) corev1.PullPolicy {
	if strings.Contains(image, "@") {
		return corev1.PullIfNotPresent
	}
	last := image[strings.LastIndex(image, "/")+1:]
	if !strings.Contains(last, ":") || strings.HasSuffix(last, ":latest") {
		return corev1.PullAlways
	}
	return corev1.PullIfNotPresent
}

func defaultPodSpec(s *corev1.PodSpec) {
	if s.RestartPolicy == "" {
		s.RestartPolicy = corev1.RestartPolicyAlways
	}
	if s.DNSPolicy == "" {
		s.DNSPolicy = corev1.DNSClusterFirst
	}
	if s.SchedulerName == "" {
		s.SchedulerName = corev1.DefaultSchedulerName
	}
	if s.TerminationGracePeriodSeconds == nil {
		s.TerminationGracePeriodSeconds = ptr.To(int64(30))
	}
	if s.SecurityContext == nil {
		s.SecurityContext = &corev1.PodSecurityContext{}
	}
	defC := func(cs []corev1.Container) {
		for i := range cs {
			c := &cs[i]
			if c.ImagePullPolicy == "" {
				c.ImagePullPolicy = pullPolicyFor(c.Image)
			}
			if c.TerminationMessagePath == "" {
				c.TerminationMessagePath = corev1.TerminationMessagePathDefault
			}
			if c.TerminationMessagePolicy == "" {
				c.TerminationMessagePolicy = corev1.TerminationMessageReadFile
			}
			for j := range c.Ports {
				if c.Ports[j].Protocol == "" {
					c.Ports[j].Protocol = corev1.ProtocolTCP
				}
			}
		}
	}
	defC(s.InitContainers)
	defC(s.Containers)
}

// Normalize applies the defaults the real API server would apply.
func Normalize(obj k8sruntime.Object) {
	switch o := obj.(type) {
	case *corev1.Pod:
		defaultPodSpec(&o.Spec)
	case *appsv1.Deployment:
		if o.Spec.Replicas == nil {
			o.Spec.Replicas = ptr.To(int32(1))
		}
		if o.Spec.RevisionHistoryLimit == nil {
			o.Spec.RevisionHistoryLimit = ptr.To(int32(10))
		}
		if o.Spec.ProgressDeadlineSeconds == nil {
			o.Spec.ProgressDeadlineSeconds = ptr.To(int32(600))
		}
		if o.Spec.Strategy.Type == "" {
			o.Spec.Strategy.Type = appsv1.RollingUpdateDeploymentStrategyType
		}
		if o.Spec.Strategy.Type == appsv1.RollingUpdateDeploymentStrategyType && o.Spec.Strategy.RollingUpdate == nil {
			mu, ms := intstr.FromString("25%"), intstr.FromString("25%")
			o.Spec.Strategy.RollingUpdate = &appsv1.RollingUpdateDeployment{MaxUnavailable: &mu, MaxSurge: &ms}
		}
		defaultPodSpec(&o.Spec.Template.Spec)
	case *appsv1.ReplicaSet:
		if o.Spec.Replicas == nil {
			o.Spec.Replicas = ptr.To(int32(1))
		}
		defaultPodSpec(&o.Spec.Template.Spec)
	case *appsv1.StatefulSet:
		if o.Spec.Replicas == nil {
			o.Spec.Replicas = ptr.To(int32(1))
		}
		if o.Spec.PodManagementPolicy == "" {
			o.Spec.PodManagementPolicy = appsv1.OrderedReadyPodManagement
		}
		if o.Spec.UpdateStrategy.Type == "" {
			o.Spec.UpdateStrategy.Type = appsv1.RollingUpdateStatefulSetStrategyType
		}
		if o.Spec.RevisionHistoryLimit == nil {
			o.Spec.RevisionHistoryLimit = ptr.To(int32(10))
		}
		defaultPodSpec(&o.Spec.Template.Spec)
	case *appsv1.DaemonSet:
		if o.Spec.UpdateStrategy.Type == "" {
			o.Spec.UpdateStrategy.Type = appsv1.RollingUpdateDaemonSetStrategyType
		}
		if o.Spec.RevisionHistoryLimit == nil {
			o.Spec.RevisionHistoryLimit = ptr.To(int32(10))
		}
		defaultPodSpec(&o.Spec.Template.Spec)
	case *batchv1.Job:
		if o.Spec.Completions == nil {
			o.Spec.Completions = ptr.To(int32(1))
		}
		if o.Spec.Parallelism == nil {
			o.Spec.Parallelism = ptr.To(int32(1))
		}
		if o.Spec.BackoffLimit == nil {
			o.Spec.BackoffLimit = ptr.To(int32(6))
		}
		defaultPodSpec(&o.Spec.Template.Spec)
	case *batchv1.CronJob:
		if o.Spec.ConcurrencyPolicy == "" {
			o.Spec.ConcurrencyPolicy = batchv1.AllowConcurrent
		}
		defaultPodSpec(&o.Spec.JobTemplate.Spec.Template.Spec)
	case *corev1.Service:
		if o.Spec.Type == "" {
			o.Spec.Type = corev1.ServiceTypeClusterIP
		}
		if o.Spec.SessionAffinity == "" {
			o.Spec.SessionAffinity = corev1.ServiceAffinityNone
		}
		for i := range o.Spec.Ports {
			p := &o.Spec.Ports[i]
			if p.Protocol == "" {
				p.Protocol = corev1.ProtocolTCP
			}
			if p.TargetPort == (intstr.IntOrString{}) {
				p.TargetPort = intstr.IntOrString{Type: intstr.Int, IntVal: p.Port}
			}
		}
	case *corev1.Secret:
		if o.Type == "" {
			o.Type = corev1.SecretTypeOpaque
		}
		if len(o.StringData) > 0 {
			if o.Data == nil {
				o.Data = map[string][]byte{}
			}
			for k, v := range o.StringData {
				o.Data[k] = []byte(v)
			}
			o.StringData = nil
		}
	case *corev1.Namespace:
		if len(o.Spec.Finalizers) == 0 {
			o.Spec.Finalizers = []corev1.FinalizerName{corev1.FinalizerKubernetes}
		}
	case *corev1.PersistentVolumeClaim:
		if o.Spec.VolumeMode == nil {
			o.Spec.VolumeMode = ptr.To(corev1.PersistentVolumeFilesystem)
		}
	}
}

// ---------------------------------------------------------------- validation

func validProtocol(p corev1.Protocol) bool {
	return p == corev1.ProtocolTCP || p == corev1.ProtocolUDP || p == corev1.ProtocolSCTP
}

func validateContainers(cs []corev1.Container, fp *field.Path, vols, seen map[string]bool) field.ErrorList {
	var errs field.ErrorList
	for i, c := range cs {
		cp := fp.Index(i)
		if c.Name == "" {
			errs = append(errs, field.Required(cp.Child("name"), ""))
		} else {
			for _, m := range validation.IsDNS1123Label(c.Name) {
				errs = append(errs, field.Invalid(cp.Child("name"), c.Name, m))
			}
			if seen[c.Name] {
				errs = append(errs, field.Duplicate(cp.Child("name"), c.Name))
			}
			seen[c.Name] = true
		}
		if strings.TrimSpace(c.Image) == "" {
			errs = append(errs, field.Required(cp.Child("image"), ""))
		}
		for j, port := range c.Ports {
			pp := cp.Child("ports").Index(j)
			for _, m := range validation.IsValidPortNum(int(port.ContainerPort)) {
				errs = append(errs, field.Invalid(pp.Child("containerPort"), port.ContainerPort, m))
			}
			if !validProtocol(port.Protocol) {
				errs = append(errs, field.NotSupported(pp.Child("protocol"), port.Protocol, []string{"TCP", "UDP", "SCTP"}))
			}
		}
		for j, e := range c.Env {
			for _, m := range validation.IsEnvVarName(e.Name) {
				errs = append(errs, field.Invalid(cp.Child("env").Index(j).Child("name"), e.Name, m))
			}
		}
		for j, vm := range c.VolumeMounts {
			vp := cp.Child("volumeMounts").Index(j)
			if !vols[vm.Name] {
				errs = append(errs, field.NotFound(vp.Child("name"), vm.Name))
			}
			if vm.MountPath == "" {
				errs = append(errs, field.Required(vp.Child("mountPath"), ""))
			}
		}
	}
	return errs
}

func validatePodSpec(spec *corev1.PodSpec, fp *field.Path) field.ErrorList {
	var errs field.ErrorList
	if len(spec.Containers) == 0 {
		errs = append(errs, field.Required(fp.Child("containers"), ""))
	}
	vols := map[string]bool{}
	for i, v := range spec.Volumes {
		vp := fp.Child("volumes").Index(i).Child("name")
		if v.Name == "" {
			errs = append(errs, field.Required(vp, ""))
			continue
		}
		for _, m := range validation.IsDNS1123Label(v.Name) {
			errs = append(errs, field.Invalid(vp, v.Name, m))
		}
		if vols[v.Name] {
			errs = append(errs, field.Duplicate(vp, v.Name))
		}
		vols[v.Name] = true
	}
	seen := map[string]bool{}
	errs = append(errs, validateContainers(spec.InitContainers, fp.Child("initContainers"), vols, seen)...)
	errs = append(errs, validateContainers(spec.Containers, fp.Child("containers"), vols, seen)...)
	switch spec.RestartPolicy {
	case corev1.RestartPolicyAlways, corev1.RestartPolicyOnFailure, corev1.RestartPolicyNever:
	default:
		errs = append(errs, field.NotSupported(fp.Child("restartPolicy"), spec.RestartPolicy, []string{"Always", "OnFailure", "Never"}))
	}
	return errs
}

func validateWorkload(sel *metav1.LabelSelector, tmpl *corev1.PodTemplateSpec, replicas *int32, fp *field.Path) field.ErrorList {
	var errs field.ErrorList
	if sel == nil {
		errs = append(errs, field.Required(fp.Child("selector"), ""))
	} else if len(sel.MatchLabels)+len(sel.MatchExpressions) == 0 {
		errs = append(errs, field.Invalid(fp.Child("selector"), sel, "empty selector is not valid"))
	} else if s, err := metav1.LabelSelectorAsSelector(sel); err != nil {
		errs = append(errs, field.Invalid(fp.Child("selector"), sel, err.Error()))
	} else if !s.Matches(labels.Set(tmpl.Labels)) {
		errs = append(errs, field.Invalid(fp.Child("template", "metadata", "labels"), tmpl.Labels, "`selector` does not match template `labels`"))
	}
	if replicas != nil && *replicas < 0 {
		errs = append(errs, field.Invalid(fp.Child("replicas"), *replicas, "must be greater than or equal to 0"))
	}
	errs = append(errs, metav1validation.ValidateLabels(tmpl.Labels, fp.Child("template", "metadata", "labels"))...)
	errs = append(errs, validatePodSpec(&tmpl.Spec, fp.Child("template", "spec"))...)
	return errs
}

func validateJobTemplate(spec *batchv1.JobSpec, fp *field.Path) field.ErrorList {
	errs := validatePodSpec(&spec.Template.Spec, fp.Child("template", "spec"))
	if rp := spec.Template.Spec.RestartPolicy; rp != corev1.RestartPolicyOnFailure && rp != corev1.RestartPolicyNever {
		errs = append(errs, field.NotSupported(fp.Child("template", "spec", "restartPolicy"), rp, []string{"OnFailure", "Never"}))
	}
	for name, v := range map[string]*int32{"completions": spec.Completions, "parallelism": spec.Parallelism, "backoffLimit": spec.BackoffLimit} {
		if v != nil && *v < 0 {
			errs = append(errs, field.Invalid(fp.Child(name), *v, "must be greater than or equal to 0"))
		}
	}
	return errs
}

func validateService(svc *corev1.Service) field.ErrorList {
	var errs field.ErrorList
	fp := field.NewPath("spec")
	switch svc.Spec.Type {
	case corev1.ServiceTypeClusterIP, corev1.ServiceTypeNodePort, corev1.ServiceTypeLoadBalancer:
		if len(svc.Spec.Ports) == 0 && svc.Spec.ClusterIP != corev1.ClusterIPNone {
			errs = append(errs, field.Required(fp.Child("ports"), ""))
		}
	case corev1.ServiceTypeExternalName:
		if svc.Spec.ExternalName == "" {
			errs = append(errs, field.Required(fp.Child("externalName"), ""))
		}
	default:
		errs = append(errs, field.NotSupported(fp.Child("type"), svc.Spec.Type, []string{"ClusterIP", "NodePort", "LoadBalancer", "ExternalName"}))
	}
	seen := map[string]bool{}
	for i, p := range svc.Spec.Ports {
		pp := fp.Child("ports").Index(i)
		if len(svc.Spec.Ports) > 1 && p.Name == "" {
			errs = append(errs, field.Required(pp.Child("name"), "name is required when there is more than one port"))
		}
		if p.Name != "" {
			for _, m := range validation.IsDNS1123Label(p.Name) {
				errs = append(errs, field.Invalid(pp.Child("name"), p.Name, m))
			}
		}
		for _, m := range validation.IsValidPortNum(int(p.Port)) {
			errs = append(errs, field.Invalid(pp.Child("port"), p.Port, m))
		}
		if !validProtocol(p.Protocol) {
			errs = append(errs, field.NotSupported(pp.Child("protocol"), p.Protocol, []string{"TCP", "UDP", "SCTP"}))
		}
		if p.TargetPort.Type == intstr.Int {
			if p.TargetPort.IntVal != 0 {
				for _, m := range validation.IsValidPortNum(int(p.TargetPort.IntVal)) {
					errs = append(errs, field.Invalid(pp.Child("targetPort"), p.TargetPort.IntVal, m))
				}
			}
		} else {
			for _, m := range validation.IsValidPortName(p.TargetPort.StrVal) {
				errs = append(errs, field.Invalid(pp.Child("targetPort"), p.TargetPort.StrVal, m))
			}
		}
		if p.NodePort != 0 && (p.NodePort < 30000 || p.NodePort > 32767) {
			errs = append(errs, field.Invalid(pp.Child("nodePort"), p.NodePort, "provided port is not in the valid range. The range of valid ports is 30000-32767"))
		}
		key := fmt.Sprintf("%d/%s", p.Port, p.Protocol)
		if seen[key] {
			errs = append(errs, field.Duplicate(pp, key))
		}
		seen[key] = true
	}
	return errs
}

func validateKeys(keys []string, fp *field.Path) field.ErrorList {
	var errs field.ErrorList
	for _, k := range keys {
		for _, m := range validation.IsConfigMapKey(k) {
			errs = append(errs, field.Invalid(fp, k, m))
		}
	}
	return errs
}

// ValidateTyped performs kind-specific validation on a decoded typed object.
func ValidateTyped(obj k8sruntime.Object) field.ErrorList {
	fp := field.NewPath("spec")
	switch o := obj.(type) {
	case *corev1.Pod:
		return validatePodSpec(&o.Spec, fp)
	case *appsv1.Deployment:
		return validateWorkload(o.Spec.Selector, &o.Spec.Template, o.Spec.Replicas, fp)
	case *appsv1.ReplicaSet:
		return validateWorkload(o.Spec.Selector, &o.Spec.Template, o.Spec.Replicas, fp)
	case *appsv1.StatefulSet:
		return validateWorkload(o.Spec.Selector, &o.Spec.Template, o.Spec.Replicas, fp)
	case *appsv1.DaemonSet:
		return validateWorkload(o.Spec.Selector, &o.Spec.Template, nil, fp)
	case *batchv1.Job:
		return validateJobTemplate(&o.Spec, fp)
	case *batchv1.CronJob:
		errs := validateJobTemplate(&o.Spec.JobTemplate.Spec, fp.Child("jobTemplate", "spec"))
		if f := strings.Fields(o.Spec.Schedule); !strings.HasPrefix(o.Spec.Schedule, "@") && len(f) != 5 {
			errs = append(errs, field.Invalid(fp.Child("schedule"), o.Spec.Schedule, "must be a valid cron expression with 5 fields"))
		}
		return errs
	case *corev1.Service:
		return validateService(o)
	case *corev1.ConfigMap:
		var keys []string
		for k := range o.Data {
			keys = append(keys, k)
		}
		for k := range o.BinaryData {
			keys = append(keys, k)
		}
		return validateKeys(keys, field.NewPath("data"))
	case *corev1.Secret:
		var keys []string
		for k := range o.Data {
			keys = append(keys, k)
		}
		return validateKeys(keys, field.NewPath("data"))
	case *corev1.PersistentVolumeClaim:
		var errs field.ErrorList
		if len(o.Spec.AccessModes) == 0 {
			errs = append(errs, field.Required(fp.Child("accessModes"), ""))
		}
		if _, ok := o.Spec.Resources.Requests[corev1.ResourceStorage]; !ok {
			errs = append(errs, field.Required(fp.Child("resources", "requests", "storage"), ""))
		}
		return errs
	case *autoscalingv2.HorizontalPodAutoscaler:
		var errs field.ErrorList
		if o.Spec.MaxReplicas < 1 {
			errs = append(errs, field.Invalid(fp.Child("maxReplicas"), o.Spec.MaxReplicas, "must be greater than 0"))
		}
		if o.Spec.ScaleTargetRef.Name == "" {
			errs = append(errs, field.Required(fp.Child("scaleTargetRef", "name"), ""))
		}
		return errs
	}
	return nil
}
