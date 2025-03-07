// Copyright 2021 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controllers

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/go-logr/logr"
	snapv1 "github.com/kubernetes-csi/external-snapshotter/v2/pkg/apis/volumesnapshot/v1beta1"
	"google.golang.org/grpc"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"

	commonv1alpha1 "github.com/GoogleCloudPlatform/elcarro-oracle-operator/common/api/v1alpha1"
	"github.com/GoogleCloudPlatform/elcarro-oracle-operator/oracle/api/v1alpha1"
	capb "github.com/GoogleCloudPlatform/elcarro-oracle-operator/oracle/pkg/agents/config_agent/protos"
	"github.com/GoogleCloudPlatform/elcarro-oracle-operator/oracle/pkg/agents/consts"
	"github.com/GoogleCloudPlatform/elcarro-oracle-operator/oracle/pkg/database/common"
)

const (
	platformGCP                            = "GCP"
	platformBareMetal                      = "BareMetal"
	platformMinikube                       = "Minikube"
	platformKind                           = "Kind"
	defaultStorageClassNameGCP             = "csi-gce-pd"
	defaultVolumeSnapshotClassNameGCP      = "csi-gce-pd-snapshot-class"
	defaultStorageClassNameBM              = "csi-trident"
	defaultVolumeSnapshotClassNameBM       = "csi-trident-snapshot-class"
	defaultStorageClassNameMinikube        = "csi-hostpath-sc"
	defaultVolumeSnapshotClassNameMinikube = "csi-hostpath-snapclass"

	configAgentName = "config-agent"
	// OperatorName is the default operator name.
	OperatorName                = "operator"
	scriptDir                   = "/agents"
	defaultUID                  = int64(54321)
	defaultGID                  = int64(54322)
	safeMinMemoryForDBContainer = "4.0Gi"
)

var (
	sourceCidrRanges = []string{"0.0.0.0/0"}
	defaultDiskSize  = resource.MustParse("100Gi")
	dialTimeout      = 3 * time.Minute
	configList       = []string{configAgentName, OperatorName}
)

type platformConfig struct {
	storageClassName        string
	volumeSnapshotClassName string
}

func getPlatformConfig(p string) (*platformConfig, error) {
	switch p {
	case platformGCP:
		return &platformConfig{
			storageClassName:        defaultStorageClassNameGCP,
			volumeSnapshotClassName: defaultVolumeSnapshotClassNameGCP,
		}, nil
	case platformBareMetal:
		return &platformConfig{
			storageClassName:        defaultStorageClassNameBM,
			volumeSnapshotClassName: defaultVolumeSnapshotClassNameBM,
		}, nil
	case platformMinikube, platformKind:
		return &platformConfig{
			storageClassName:        defaultStorageClassNameMinikube,
			volumeSnapshotClassName: defaultVolumeSnapshotClassNameMinikube,
		}, nil
	default:
		return nil, fmt.Errorf("the current release doesn't support deployment platform %q", p)
	}
}

func (pc *platformConfig) finalStorageClassName(config *v1alpha1.Config) string {
	storageClassName := pc.storageClassName

	// Override if explicitly requested by the Custom/Global Config.
	// If it's not requested in the Global Config, return "",
	// which at this point would constitute an error.
	// (no platform specific default and no override).
	if config != nil {
		storageClassName = config.Spec.StorageClass
	}

	return storageClassName
}

func (pc *platformConfig) finalVolumeSnapshotClassName(config *v1alpha1.Config) string {
	volumeSnapshotClassName := pc.volumeSnapshotClassName

	// Override if explicitly requested by the Custom/Global Config.
	// If it's not requested in the Global Config, return "",
	// which at this point would constitute an error.
	// (no platform specific default and no override).
	if config != nil {
		volumeSnapshotClassName = config.Spec.VolumeSnapshotClass
	}

	return volumeSnapshotClassName
}

// NewSvc returns the service for the database.
func NewSvc(inst *v1alpha1.Instance, scheme *runtime.Scheme, lb string) (*corev1.Service, error) {
	if len(inst.Spec.SourceCidrRanges) > 0 {
		sourceCidrRanges = inst.Spec.SourceCidrRanges
	}
	var svcAnnotations map[string]string

	lbType := corev1.ServiceTypeLoadBalancer
	svcNameFull := fmt.Sprintf(SvcName, inst.Name)
	if lb == "node" {
		lbType = corev1.ServiceTypeNodePort
		svcNameFull = svcNameFull + "-" + lb
	} else {
		networkOpts := inst.Spec.DBNetworkServiceOptions
		if networkOpts != nil && networkOpts.GCP.LoadBalancerType == "Internal" {
			svcAnnotations = map[string]string{
				"cloud.google.com/load-balancer-type": "Internal",
			}
		}
	}

	svc := &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: svcNameFull, Namespace: inst.Namespace, Annotations: svcAnnotations},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"instance": inst.Name},
			Ports: []corev1.ServicePort{
				{
					Name:       "secure-listener",
					Protocol:   "TCP",
					Port:       consts.SecureListenerPort,
					TargetPort: intstr.FromInt(consts.SecureListenerPort),
				},
				{
					Name:       "ssl-listener",
					Protocol:   "TCP",
					Port:       consts.SSLListenerPort,
					TargetPort: intstr.FromInt(consts.SSLListenerPort),
				},
			},
			Type: lbType,
			// LoadBalancerSourceRanges: sourceCidrRanges,
		},
	}

	// Set the Instance resource to own the Service resource.
	if err := ctrl.SetControllerReference(inst, svc, scheme); err != nil {
		return svc, err
	}

	return svc, nil
}

// NewDBDaemonSvc returns the service for the database daemon server.
func NewDBDaemonSvc(inst *v1alpha1.Instance, scheme *runtime.Scheme) (*corev1.Service, error) {
	svc := &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf(DbdaemonSvcName, inst.Name), Namespace: inst.Namespace},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"instance": inst.Name},
			Ports: []corev1.ServicePort{
				{
					Name:       "dbdaemon",
					Protocol:   "TCP",
					Port:       consts.DefaultDBDaemonPort,
					TargetPort: intstr.FromInt(consts.DefaultDBDaemonPort),
				},
			},
			Type: corev1.ServiceTypeClusterIP,
		},
	}

	// Set the Instance resource to own the Service resource.
	if err := ctrl.SetControllerReference(inst, svc, scheme); err != nil {
		return svc, err
	}

	return svc, nil
}

// NewAgentSvc returns the service for the agent.
func NewAgentSvc(inst *v1alpha1.Instance, scheme *runtime.Scheme) (*corev1.Service, error) {
	ports := []corev1.ServicePort{
		{
			Name:       configAgentName,
			Protocol:   "TCP",
			Port:       consts.DefaultConfigAgentPort,
			TargetPort: intstr.FromInt(consts.DefaultConfigAgentPort),
		},
	}
	for service, enabled := range inst.Spec.Services {
		switch service {
		case commonv1alpha1.Monitoring:
			if enabled {
				ports = append(ports, corev1.ServicePort{
					Name:     consts.MonitoringAgentName,
					Protocol: "TCP",
					Port:     consts.DefaultMonitoringAgentPort,
				})
			}
		}
	}
	svc := &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf(AgentSvcName, inst.Name),
			Namespace: inst.Namespace,
			Labels:    map[string]string{"app": "agent-svc"},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"instance-agent": fmt.Sprintf("%s-agent", inst.Name)},
			Ports:    ports,
			Type:     corev1.ServiceTypeClusterIP,
		},
	}

	// Set the Instance resource to own the Service resource.
	if err := ctrl.SetControllerReference(inst, svc, scheme); err != nil {
		return svc, err
	}

	return svc, nil
}

// SvcURL returns the URL for the database service.
func SvcURL(svc *corev1.Service, port int32) string {
	// Unset if not present: state to reflect what's observed.
	if len(svc.Status.LoadBalancer.Ingress) == 0 {
		return ""
	}

	hostName := svc.Status.LoadBalancer.Ingress[0].Hostname
	if hostName == "" {
		hostName = svc.Status.LoadBalancer.Ingress[0].IP
	}

	return net.JoinHostPort(hostName, fmt.Sprintf("%d", port))
}

// NewConfigMap returns the config map for database env variables.
func NewConfigMap(inst *v1alpha1.Instance, scheme *runtime.Scheme, cmName string) (*corev1.ConfigMap, error) {
	cm := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: inst.Namespace},
		Data: map[string]string{
			"SCRIPTS_DIR":           scriptDir,
			"INSTALL_DIR":           "/stage",
			"HEALTHCHECK_DB_SCRIPT": "health-check-db.sh",
			"ORACLE_BASE":           common.GetSourceOracleBase(inst.Spec.Version),
			"ORACLE_INV":            common.GetSourceOracleInventory(inst.Spec.Version),
			"ORACLE_HOME":           common.GetSourceOracleHome(inst.Spec.Version),
			"LD_LIBRARY_PATH":       fmt.Sprintf("export LD_LIBRARY_PATH=%s/lib:/usr/lib\n", common.GetSourceOracleHome(inst.Spec.Version)),
		},
	}

	// Set the Instance resource to own the ConfigMap resource.
	if err := ctrl.SetControllerReference(inst, cm, scheme); err != nil {
		return cm, err
	}

	return cm, nil
}

// NewSts returns the statefulset for the database pod.
func NewSts(sp StsParams, pvcs []corev1.PersistentVolumeClaim, podTemplate corev1.PodTemplateSpec) (*appsv1.StatefulSet, error) {
	var replicas int32 = 1
	sts := &appsv1.StatefulSet{
		// It looks like the version needs to be explicitly set to avoid the
		// "incorrect version specified in apply patch" error.
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
		ObjectMeta: metav1.ObjectMeta{Name: sp.StsName, Namespace: sp.Inst.Namespace},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			// UpdateStrategy: appsv1.StatefulSetUpdateStrategy{Type: appsv1.RollingUpdateStatefulSetStrategyType},
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"instance": sp.Inst.Name, "statefulset": sp.StsName},
			},
			Template: podTemplate,
			// Do we need a pointer to a service in a StatefulSet?
			// ServiceName:          sp.svcName,
			VolumeClaimTemplates: pvcs,
		},
	}

	// Set the Instance resource to own the StatefulSet resource.
	if err := ctrl.SetControllerReference(sp.Inst, sts, sp.Scheme); err != nil {
		return sts, err
	}

	return sts, nil
}

// GetLogLevelArgs returns agent args for log level.
func GetLogLevelArgs(config *v1alpha1.Config) map[string][]string {
	agentArgs := make(map[string][]string)
	if config == nil {
		return agentArgs
	}

	for _, name := range configList {
		args := []string{}
		if len(config.Spec.LogLevel[name]) > 0 {
			args = append(args, fmt.Sprintf("--v=%s", config.Spec.LogLevel[name]))
		}
		agentArgs[name] = args
	}

	return agentArgs
}

// NewAgentDeployment returns the agent deployment.
func NewAgentDeployment(agentDeployment AgentDeploymentParams) (*appsv1.Deployment, error) {
	var replicas int32 = 1
	instlabels := map[string]string{"instance": agentDeployment.Inst.Name}
	labels := map[string]string{"instance-agent": fmt.Sprintf("%s-agent", agentDeployment.Inst.Name), "deployment": agentDeployment.Name}

	configAgentArgs := []string{
		fmt.Sprintf("--port=%d", consts.DefaultConfigAgentPort),
		fmt.Sprintf("--dbservice=%s", fmt.Sprintf(DbdaemonSvcName, agentDeployment.Inst.Name)),
		fmt.Sprintf("--dbport=%d", consts.DefaultDBDaemonPort),
	}

	monitoringAgentArgs := []string{
		fmt.Sprintf("--dbservice=%s", fmt.Sprintf(DbdaemonSvcName, agentDeployment.Inst.Name)),
		fmt.Sprintf("--dbport=%d", consts.DefaultDBDaemonPort),
	}

	if len(agentDeployment.Args[configAgentName]) > 0 {
		for _, arg := range agentDeployment.Args[configAgentName] {
			configAgentArgs = append(configAgentArgs, arg)
		}
	}

	// Kind cluster can only use local images
	imagePullPolicy := corev1.PullAlways
	if agentDeployment.Config != nil && agentDeployment.Config.Spec.Platform == platformKind {
		imagePullPolicy = corev1.PullIfNotPresent
	}

	containers := []corev1.Container{
		{
			Name:    configAgentName,
			Image:   agentDeployment.Images["config"],
			Command: []string{"/configagent"},
			Args:    configAgentArgs,
			Ports: []corev1.ContainerPort{
				{Name: "ca-port", Protocol: "TCP", ContainerPort: consts.DefaultConfigAgentPort},
			},
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: &agentDeployment.PrivEscalation,
			},
			ImagePullPolicy: imagePullPolicy,
		},
	}
	agentDeployment.Log.V(2).Info("enabling services: ", "services", agentDeployment.Services)
	for _, s := range agentDeployment.Services {
		switch s {
		case commonv1alpha1.Monitoring:
			containers = append(containers, corev1.Container{
				Name:    consts.MonitoringAgentName,
				Image:   agentDeployment.Images["monitoring"],
				Command: []string{"/monitoring_agent"},
				Args:    monitoringAgentArgs,
				Ports: []corev1.ContainerPort{
					{
						Name:          "oe-port",
						Protocol:      "TCP",
						ContainerPort: consts.DefaultMonitoringAgentPort,
					},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &agentDeployment.PrivEscalation,
				},
				ImagePullPolicy: imagePullPolicy,
			})
		default:
			agentDeployment.Log.V(2).Info("unsupported service: ", "service", s)
		}
	}

	podSpec := corev1.PodSpec{
		SecurityContext: &corev1.PodSecurityContext{},
		Containers:      containers,
		// Add pod affinity for agent pod, so that k8s will try to schedule the agent pod
		// to the same node where the paired DB pod is located. In this way, we can avoid
		// unnecessary cross node communication.
		Affinity: &corev1.Affinity{
			PodAffinity: &corev1.PodAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
					{
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: instlabels,
						},
						Namespaces:  []string{agentDeployment.Inst.Namespace},
						TopologyKey: "kubernetes.io/hostname",
					},
				},
			},
		},
	}

	template := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:    labels,
			Namespace: agentDeployment.Inst.Namespace,
		},
		Spec: podSpec,
	}

	deployment := &appsv1.Deployment{
		// It looks like the version needs to be explicitly set to avoid the
		// "incorrect version specified in apply patch" error.
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: agentDeployment.Name, Namespace: agentDeployment.Inst.Namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: template,
		},
	}

	if err := ctrl.SetControllerReference(agentDeployment.Inst, deployment, agentDeployment.Scheme); err != nil {
		return deployment, err
	}
	return deployment, nil
}

func findDiskSize(diskName string, sp StsParams) resource.Quantity {
	spec, exists := defaultDiskSpecs[diskName]
	if !exists {
		sp.Log.Info("no default volume bind with diskName %q, returns default disk size %q", diskName, defaultDiskSize)
		return defaultDiskSize
	}

	if sp.Disks != nil {
		for _, d := range sp.Disks {
			if d.Name == diskName && !d.Size.IsZero() {
				sp.Log.Info("returns size with an instance-level requested size", "diskName", diskName, "mount", defaultDiskMountLocations[spec.Name], "requestedDiskSize", d.Size)
				return d.Size
			}
		}
	}

	if sp.Config != nil {
		for _, d := range sp.Config.Spec.Disks {
			if d.Name == diskName && !d.Size.IsZero() {
				sp.Log.Info("returns size with the customer provided (global preference) numbers", "mount", defaultDiskMountLocations[spec.Name], "diskName", diskName, "diskSizes", d.Size)
				return d.Size
			}
		}
	}
	sp.Log.Info("returns size with default numbers", "diskName", diskName, "mount", defaultDiskMountLocations[spec.Name], "diskSizes", spec.Size)
	return spec.Size
}

// NewPVCs returns PVCs.
func NewPVCs(sp StsParams) ([]corev1.PersistentVolumeClaim, error) {
	var pvcs []corev1.PersistentVolumeClaim

	for _, diskSpec := range sp.Disks {
		rl := corev1.ResourceList{corev1.ResourceStorage: findDiskSize(diskSpec.Name, sp)}
		pvcName, mount := GetPVCNameAndMount(sp.Inst.Name, diskSpec.Name)
		var pvc corev1.PersistentVolumeClaim

		// Determine storage class (from disk spec or config)
		storageClass, err := ConfigAttribute("StorageClass", diskSpec.StorageClass, sp.Config)
		if err != nil || storageClass == "" {
			return nil, fmt.Errorf("failed to identify a storageClassName for disk %q", diskSpec.Name)
		}
		sp.Log.Info("storage class identified", "disk", diskSpec.Name, "StorageClass", storageClass)

		pvc = corev1.PersistentVolumeClaim{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
			ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: sp.Inst.Namespace},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{"ReadWriteOnce"},
				Resources:        corev1.ResourceRequirements{Requests: rl},
				StorageClassName: func() *string { s := storageClass; return &s }(),
			},
		}

		if sp.Restore != nil && sp.Restore.BackupID != "" {
			sp.Log.Info("starting a restore process for disk", "mount", mount)
			pvc.Spec.DataSource = &corev1.TypedLocalObjectReference{
				APIGroup: func() *string { s := string("snapshot.storage.k8s.io"); return &s }(),
				Kind:     "VolumeSnapshot",
				Name:     fmt.Sprintf("%s-%s", sp.Restore.BackupID, mount),
			}
		} else {
			sp.Log.Info("starting a provisioning process for disk", "mount", mount)
		}

		pvcs = append(pvcs, pvc)
	}

	return pvcs, nil
}

func buildPVCMounts(sp StsParams) []corev1.VolumeMount {
	var diskMounts []corev1.VolumeMount

	for _, diskSpec := range sp.Disks {
		pvcName, mount := GetPVCNameAndMount(sp.Inst.Name, diskSpec.Name)
		diskMounts = append(diskMounts, corev1.VolumeMount{
			Name:      pvcName,
			MountPath: fmt.Sprintf("/%s", mount),
		})
	}

	return diskMounts
}

// NewPodTemplate returns the pod template for the database statefulset.
func NewPodTemplate(sp StsParams, cdbName, DBDomain string) corev1.PodTemplateSpec {
	labels := map[string]string{
		"instance":    sp.Inst.Name,
		"statefulset": sp.StsName,
		"app":         DatabasePodAppLabel,
	}

	minMemoryForDBContainer := safeMinMemoryForDBContainer
	if sp.Inst.Spec.MinMemoryForDBContainer != "" {
		minMemoryForDBContainer = sp.Inst.Spec.MinMemoryForDBContainer
		sp.Log.Info("NewPodTemplate: replacing", "SafeMinMemoryForDBContainer", safeMinMemoryForDBContainer, "sp.Inst.Spec.MinMemoryForDBContainer", sp.Inst.Spec.MinMemoryForDBContainer)
	}

	// Kind cluster can only use local images
	imagePullPolicy := corev1.PullAlways
	if sp.Config != nil && sp.Config.Spec.Platform == platformKind {
		imagePullPolicy = corev1.PullIfNotPresent
	}

	sp.Log.Info("NewPodTemplate: creating new template with service image", "image", sp.Images["service"])
	dataDiskPVC, dataDiskMountName := GetPVCNameAndMount(sp.Inst.Name, "DataDisk")
	containers := []corev1.Container{
		{
			Name: "oracledb",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse(minMemoryForDBContainer),
				},
			},
			Image:   sp.Images["service"],
			Command: []string{fmt.Sprintf("%s/init_oracle.sh", scriptDir)},
			Args:    []string{cdbName, DBDomain},
			Ports: []corev1.ContainerPort{
				{Name: "secure-listener", Protocol: "TCP", ContainerPort: consts.SecureListenerPort},
				{Name: "ssl-listener", Protocol: "TCP", ContainerPort: consts.SSLListenerPort},
			},
			VolumeMounts: append([]corev1.VolumeMount{
				{Name: "var-tmp", MountPath: "/var/tmp"},
				{Name: "agent-repo", MountPath: "/agents"},
			},
				buildPVCMounts(sp)...),
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: &sp.PrivEscalation,
			},
			EnvFrom: []corev1.EnvFromSource{
				{
					ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: sp.ConfigMap.ObjectMeta.Name}},
				},
			},
			ImagePullPolicy: imagePullPolicy,
		},
		{
			Name:    "dbdaemon",
			Image:   sp.Images["service"],
			Command: []string{"/agents/dbdaemon"},
			Args:    []string{fmt.Sprintf("--cdb_name=%s", cdbName)},
			Ports: []corev1.ContainerPort{
				{Name: "dbdaemon", Protocol: "TCP", ContainerPort: consts.DefaultDBDaemonPort},
			},
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: &sp.PrivEscalation,
			},
			VolumeMounts: append([]corev1.VolumeMount{
				{Name: "var-tmp", MountPath: "/var/tmp"},
				{Name: "agent-repo", MountPath: "/agents"},
			},
				buildPVCMounts(sp)...),
			ImagePullPolicy: imagePullPolicy,
		},
		{
			Name:    "alert-log-sidecar",
			Image:   sp.Images["logging_sidecar"],
			Command: []string{"/logging_main"},
			Args:    []string{"--logType=ALERT"},
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: &sp.PrivEscalation,
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: dataDiskPVC, MountPath: fmt.Sprintf("/%s", dataDiskMountName)},
			},
			ImagePullPolicy: imagePullPolicy,
		},
		{
			Name:    "listener-log-sidecar",
			Image:   sp.Images["logging_sidecar"],
			Command: []string{"/logging_main"},
			Args:    []string{"--logType=LISTENER"},
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: &sp.PrivEscalation,
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: dataDiskPVC, MountPath: fmt.Sprintf("/%s", dataDiskMountName)},
			},
			ImagePullPolicy: imagePullPolicy,
		},
	}
	initContainers := []corev1.Container{
		{
			Name:    "dbinit",
			Image:   sp.Images["dbinit"],
			Command: []string{"sh", "-c", "cp -r agent_repo/. /agents/ && chmod -R 750 /agents/*"},
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: &sp.PrivEscalation,
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "agent-repo", MountPath: "/agents"},
			},
			ImagePullPolicy: imagePullPolicy,
		},
	}

	volumes := []corev1.Volume{
		{
			Name:         "var-tmp",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
		{
			Name:         "agent-repo",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
	}

	var antiAffinityNamespaces []string
	if sp.Config != nil && len(sp.Config.Spec.HostAntiAffinityNamespaces) != 0 {
		antiAffinityNamespaces = sp.Config.Spec.HostAntiAffinityNamespaces
	}

	uid := sp.Inst.Spec.DatabaseUID
	if uid == nil {
		sp.Log.Info("set pod user ID to default value", "UID", defaultUID)
		// consts are not addressable
		uid = func(i int64) *int64 { return &i }(defaultUID)
	}

	gid := sp.Inst.Spec.DatabaseGID
	if gid == nil {
		sp.Log.Info("set pod group ID to default value", "GID", defaultGID)
		// consts are not addressable
		gid = func(i int64) *int64 { return &i }(defaultGID)
	}

	// for minikube/kind, the default csi-hostpath-driver mounts persistent volumes writable by root only, so explicitly
	// change owner and permissions of mounted pvs with an init container.
	if sp.Config != nil && (sp.Config.Spec.Platform == platformMinikube || sp.Config.Spec.Platform == platformKind) {
		initContainers = addHostpathInitContainer(sp, initContainers, *uid, *gid)
	}

	podSpec := corev1.PodSpec{
		SecurityContext: &corev1.PodSecurityContext{
			RunAsUser:    uid,
			RunAsGroup:   gid,
			FSGroup:      gid,
			RunAsNonRoot: func(b bool) *bool { return &b }(true),
		},
		// ImagePullSecrets: []corev1.LocalObjectReference {{Name: GcrSecretName }},
		// InitContainers: initContainers,
		Containers:            containers,
		InitContainers:        initContainers,
		ShareProcessNamespace: func(b bool) *bool { return &b }(true),
		// ServiceAccountName:
		// TerminationGracePeriodSeconds:
		// Tolerations:
		Volumes: volumes,
		Affinity: &corev1.Affinity{
			PodAntiAffinity: &corev1.PodAntiAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
					{
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"app": DatabasePodAppLabel},
						},
						Namespaces:  antiAffinityNamespaces,
						TopologyKey: "kubernetes.io/hostname",
					},
				},
			},
		},
	}

	// TODO(bdali): consider adding pod affinity, priority class name, secret mount.

	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:    labels,
			Namespace: sp.Namespace,
			// Annotations: annotations,
		},
		Spec: podSpec,
	}
}

// NewSnapshot returns the snapshot for the given pv.
func NewSnapshot(backup *v1alpha1.Backup, scheme *runtime.Scheme, pvcName, snapName, volumeSnapshotClassName string) (*snapv1.VolumeSnapshot, error) {
	snap := &snapv1.VolumeSnapshot{
		TypeMeta:   metav1.TypeMeta{APIVersion: snapv1.SchemeGroupVersion.String(), Kind: "VolumeSnapshot"},
		ObjectMeta: metav1.ObjectMeta{Name: snapName, Namespace: backup.Namespace, Labels: map[string]string{"snap": snapName}},
		Spec: snapv1.VolumeSnapshotSpec{
			Source:                  snapv1.VolumeSnapshotSource{PersistentVolumeClaimName: &pvcName},
			VolumeSnapshotClassName: func() *string { s := string(volumeSnapshotClassName); return &s }(),
		},
	}

	// Set the Instance resource to own the VolumeSnapshot resource.
	if err := ctrl.SetControllerReference(backup, snap, scheme); err != nil {
		return snap, err
	}

	return snap, nil
}

// NewSnapshot returns the snapshot for the given instance and pv.
func NewSnapshotInst(inst *v1alpha1.Instance, scheme *runtime.Scheme, pvcName, snapName, volumeSnapshotClassName string) (*snapv1.VolumeSnapshot, error) {
	snap := &snapv1.VolumeSnapshot{
		TypeMeta:   metav1.TypeMeta{APIVersion: snapv1.SchemeGroupVersion.String(), Kind: "VolumeSnapshot"},
		ObjectMeta: metav1.ObjectMeta{Name: snapName, Namespace: inst.Namespace, Labels: map[string]string{"snap": snapName}},
		Spec: snapv1.VolumeSnapshotSpec{
			Source:                  snapv1.VolumeSnapshotSource{PersistentVolumeClaimName: &pvcName},
			VolumeSnapshotClassName: func() *string { s := string(volumeSnapshotClassName); return &s }(),
		},
	}

	// Set the Instance resource to own the VolumeSnapshot resource.
	if err := ctrl.SetControllerReference(inst, snap, scheme); err != nil {
		return snap, err
	}

	return snap, nil
}

// checkStatusInstance attempts to determine a state of an database instance.
// In particular:
//   - has provisioning finished?
//   - is Instance up and accepting connection requests?
var CheckStatusInstanceFunc = func(ctx context.Context, instName, cdbName, clusterIP, DBDomain string, log logr.Logger) (string, error) {
	log.Info("resources/checkStatusInstance", "inst name", instName, "clusterIP", clusterIP)

	// Establish a connection to a Config Agent.
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	conn, err := grpc.Dial(fmt.Sprintf("%s:%d", clusterIP, consts.DefaultConfigAgentPort), grpc.WithInsecure())
	if err != nil {
		log.Error(err, "resources/checkStatusInstance: failed to create a conn via gRPC.Dial")
		return "", err
	}
	defer conn.Close()

	caClient := capb.NewConfigAgentClient(conn)
	cdOut, err := caClient.CheckStatus(ctx, &capb.CheckStatusRequest{
		Name:            instName,
		CdbName:         cdbName,
		CheckStatusType: capb.CheckStatusRequest_INSTANCE,
		DbDomain:        DBDomain,
	})
	if err != nil {
		return "", fmt.Errorf("resource/checkStatusInstance: failed on CheckStatus gRPC call: %v", err)
	}
	log.Info("resource/CheckStatusInstance: DONE with this output", "out", cdOut)

	return string(cdOut.Status), nil
}

// GetDBDomain figures out DBDomain from DBUniqueName and DBDomain.
func GetDBDomain(inst *v1alpha1.Instance) string {
	// Does DBUniqueName contain a DB Domain suffix?
	if strings.Contains(inst.Spec.DBUniqueName, ".") {
		domainFromName := strings.SplitN(inst.Spec.DBUniqueName, ".", 2)[1]
		return domainFromName
	}

	return inst.Spec.DBDomain
}

// ConfigAttribute attempts to detect what value to use for a requested
// attribute. If an explicit value is requested via the Spec,
// it's immediately returned "as is". If not, a customer global Config
// is checked and returned if set. Failing all that a platform default
// value is used for a requested attribute.
func ConfigAttribute(name, explicitRequest string, config *v1alpha1.Config) (string, error) {
	if explicitRequest != "" {
		return explicitRequest, nil
	}

	// Assume the default platform as GCP. This can be overridden via a Config.
	platform := platformGCP
	if config != nil && config.Spec.Platform != "" {
		platform = config.Spec.Platform
	}

	gc, err := getPlatformConfig(platform)
	if err != nil {
		return "", err
	}

	switch name {
	case "StorageClass":
		return gc.finalStorageClassName(config), nil
	case "VolumeSnapshotClass":
		return gc.finalVolumeSnapshotClassName(config), nil
	default:
		return "", fmt.Errorf("unknown attribute requested (presently supported: StorageClass, VolumeSnapshotClass): %q", name)
	}
}

func addHostpathInitContainer(sp StsParams, containers []corev1.Container, uid, gid int64) []corev1.Container {
	volumeMounts := buildPVCMounts(sp)
	cmd := ""
	for _, mount := range volumeMounts {
		if cmd != "" {
			cmd += " && "
		}
		cmd += fmt.Sprintf("chown %d:%d %s ", uid, gid, mount.MountPath)
	}
	sp.Log.Info("add an init container for csi-hostpath-sc type pv", "cmd", cmd)
	return append(containers, corev1.Container{
		Name:    "prepare-pv-container",
		Image:   "busybox:latest",
		Command: []string{"sh", "-c", cmd},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                func(i int64) *int64 { return &i }(0),
			RunAsGroup:               func(i int64) *int64 { return &i }(0),
			RunAsNonRoot:             func(b bool) *bool { return &b }(false),
			AllowPrivilegeEscalation: &sp.PrivEscalation,
		},
		VolumeMounts: volumeMounts,
	})
}
