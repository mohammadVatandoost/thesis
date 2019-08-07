package unikernel

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"github.com/virtual-kubelet/virtual-kubelet/trace"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	//_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
)

const (
	defaultCPUCapacity    = "20"
	defaultMemoryCapacity = "100Gi"
	defaultPodCapacity    = "20"

	namespaceKey     = "namespace"
	nameKey          = "name"
	containerNameKey = "containerName"
)

// LocalProvider implements the virtual-kubelet provider interface and runs locally, pretending to run pods
type Provider struct {
	nodeName           string
	operatingSystem    string
	internalIP         string
	daemonEndpointPort int32
	pods               map[string]*v1.Pod
	config             MockConfig
	startTime          time.Time
	notifier           func(*v1.Pod)
}
type MockConfig struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	Pods   string `json:"pods,omitempty"`
}

func NewProvider(providerConfig, nodeName, operatingSystem string, internalIP string, daemonEndpointPort int32) (*Provider, error) {

	provider := Provider{
		nodeName:           nodeName,
		operatingSystem:    operatingSystem,
		internalIP:         internalIP,
		daemonEndpointPort: daemonEndpointPort,
		pods:               make(map[string]*v1.Pod),
		config:             MockConfig{CPU: defaultCPUCapacity, Memory: defaultMemoryCapacity, Pods: defaultPodCapacity},
		startTime:          time.Now(),
		// By default notifier is set to a function which is a no-op. In the event we've implemented the PodNotifier interface,
		// it will be set, and then we'll call a real underlying implementation.
		// This makes it easier in the sense we don't need to wrap each method.
		notifier: func(*v1.Pod) {},
	}

	return &provider, nil
}

func (s *Provider) CreatePod(ctx context.Context, pod *v1.Pod) error {
	ctx, span := trace.StartSpan(ctx, "CreatePod")
	defer span.End()

	ctx = addAttributes(ctx, span, namespaceKey, pod.Namespace,nameKey, pod.Name)
	log.G(ctx).Infof("receive CreatePod %q",pod.Name)

	key,err:=buildKey(pod)
	if err!=nil{
		return err
	}

	now:=metav1.NewTime(time.Now())
	pod.Status = v1.PodStatus{
		Phase:     v1.PodRunning,
		HostIP:    "192.168.1.147",
		PodIP:     "",
		StartTime: &now,
		Conditions: []v1.PodCondition{
			{
				Type:   v1.PodInitialized,
				Status: v1.ConditionTrue,
			},
			{
				Type:   v1.PodReady,
				Status: v1.ConditionTrue,
			},
			{
				Type:   v1.PodScheduled,
				Status: v1.ConditionTrue,
			},
		},
	}
	for _, container := range pod.Spec.Containers {
		pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, v1.ContainerStatus{
			Name:         container.Name,
			Image:        container.Image,
			Ready:        true,
			RestartCount: 0,
			State: v1.ContainerState{
				Running: &v1.ContainerStateRunning{
					StartedAt: now,
				},
			},
		})
	}
	s.pods[key] = pod
	s.notifier(pod)
	return nil
}

func (s *Provider) UpdatePod(ctx context.Context, pod *v1.Pod) error {
	ctx, span := trace.StartSpan(ctx, "UpdatePod")
	defer span.End()

	// Add the pod's coordinates to the current span.
	ctx = addAttributes(ctx, span, namespaceKey, pod.Namespace, nameKey, pod.Name)

	log.G(ctx).Infof("receive UpdatePod %q", pod.Name)

	key, err := buildKey(pod)
	if err != nil {
		return err
	}

	s.pods[key] = pod
	s.notifier(pod)
	return nil
}

func (s *Provider) DeletePod(ctx context.Context, pod *v1.Pod) error {
	ctx, span := trace.StartSpan(ctx, "DeletePod")
	defer span.End()

	ctx = addAttributes(ctx, span, namespaceKey, pod.Namespace, nameKey, pod.Name)

	log.G(ctx).Infof("receive DeletePod %q", pod.Name)

	key,err:=buildKey(pod)
	if err!=nil{
		return err
	}

	if _,exists:=s.pods[key]; !exists{
		return errdefs.NotFound("pod not found")
	}
	now:=metav1.Now()
	delete(s.pods,key)
	pod.Status.Phase=v1.PodSucceeded
	pod.Status.Reason="UnikernelProviderDeleted"

	for idx:=range pod.Status.ContainerStatuses{
		pod.Status.ContainerStatuses[idx].Ready=false
		pod.Status.ContainerStatuses[idx].State=v1.ContainerState{
			Terminated: &v1.ContainerStateTerminated{Message: "Unikernel provider terminated container upon deletion",
				FinishedAt: now,
				Reason:     "UnikernelProviderPodContainerDeleted",
				StartedAt:  pod.Status.ContainerStatuses[idx].State.Running.StartedAt,
			},
		}
	}
	s.notifier(pod)
	return nil
}

func (s *Provider) GetPod(ctx context.Context, namespace, name string) (pod *v1.Pod, err error) {
	ctx, span := trace.StartSpan(ctx, "GetPod")
	defer func() {
		span.SetStatus(err)
		span.End()
	}()

	ctx = addAttributes(ctx, span, namespaceKey, namespace, nameKey, name)

	log.G(ctx).Infof("receive GetPod %q", name)

	key, err := buildKeyFromNames(namespace,name)
	if err != nil {
		return nil, err
	}

	if pod, ok := s.pods[key]; ok {
		return pod, nil
	}
	return nil, errdefs.NotFoundf("pod \"%s/%s\" is not known to the provider", namespace, name)
}

func (s *Provider) GetPodStatus(ctx context.Context, namespace, name string) (*v1.PodStatus, error) {
	ctx, span := trace.StartSpan(ctx, "GetPodStatus")
	defer span.End()

	ctx = addAttributes(ctx, span, namespaceKey, namespace, nameKey, name)

	log.G(ctx).Infof("receive GetPodStatus %q", name)

	pod, err := s.GetPod(ctx, namespace, name)

	if err != nil {
		return nil, err
	}

	return &pod.Status, nil
}

func (s *Provider) GetPods(ctx context.Context) ([]*v1.Pod, error) {
	ctx, span := trace.StartSpan(ctx, "GetPods")
	defer span.End()

	log.G(ctx).Info("receive GetPods")
	var pods []*v1.Pod
	for _, pod := range s.pods {
		pods = append(pods, pod)
	}
	return pods, nil
}

func (*Provider) GetContainerLogs(ctx context.Context, namespace, podName, containerName string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	panic("Get Container Logs")
}

func (*Provider) RunInContainer(ctx context.Context, namespace, podName, containerName string, cmd []string, attach api.AttachIO) error {
	panic("RunInContainer")
}

func (*Provider) Capacity(ctx context.Context) v1.ResourceList {

	ctx, span := trace.StartSpan(ctx, "Capacity")
	defer span.End()

	return v1.ResourceList{
		"cpu":    resource.MustParse(defaultCPUCapacity),
		"memory": resource.MustParse(defaultMemoryCapacity),
		"pods":   resource.MustParse(defaultPodCapacity),
	}
}

func (*Provider) NodeConditions(ctx context.Context) []v1.NodeCondition {
	ctx, span := trace.StartSpan(ctx, "NodeConditions")
	defer span.End()

	return []v1.NodeCondition{{
		Type:               "Ready",
		Status:             v1.ConditionTrue,
		LastHeartbeatTime:  metav1.Now(),
		LastTransitionTime: metav1.Now(),
		Reason:             "KubeletReady",
		Message:            "kubelet is ready.",
	},
	}
}

func (s *Provider) nodeAddresses() []v1.NodeAddress {
	return []v1.NodeAddress{
		{
			Type:    "InternalIP",
			Address: s.internalIP,
		},
	}
}

func (s *Provider) nodeDaemonEndpoints() v1.NodeDaemonEndpoints {
	return v1.NodeDaemonEndpoints{
		KubeletEndpoint: v1.DaemonEndpoint{
			Port: s.daemonEndpointPort,
		},
	}
}

func addAttributes(ctx context.Context, span trace.Span, attrs ...string) context.Context {
	if len(attrs)%2 == 1 {
		return ctx
	}
	for i := 0; i < len(attrs); i += 2 {
		ctx = span.WithField(ctx, attrs[i], attrs[i+1])
	}
	return ctx
}

func buildKey(pod *v1.Pod) (string, error) {
	if pod.ObjectMeta.Namespace == "" {
		return "", fmt.Errorf("pod namespace not found")
	}

	if pod.ObjectMeta.Name == "" {
		return "", fmt.Errorf("pod name not found")
	}

	return buildKeyFromNames(pod.ObjectMeta.Namespace,pod.ObjectMeta.Name)
}

func (s *Provider) NotifyPods(ctx context.Context, notifier func(*v1.Pod)) {
	s.notifier = notifier
}
func (s *Provider) capacity() v1.ResourceList {
	return v1.ResourceList{
		"cpu":    resource.MustParse(s.config.CPU),
		"memory": resource.MustParse(s.config.Memory),
		"pods":   resource.MustParse(s.config.Pods),
	}
}
func (s *Provider) ConfigureNode(ctx context.Context, n *v1.Node) {
	ctx, span := trace.StartSpan(ctx, "mock.ConfigureNode")
	defer span.End()

	n.Status.Capacity = s.capacity()
	n.Status.Allocatable = s.capacity()
	n.Status.Conditions = s.nodeConditions()
	n.Status.Addresses = s.nodeAddresses()
	n.Status.DaemonEndpoints = s.nodeDaemonEndpoints()
	os := s.operatingSystem
	if os == "" {
		os = "Linux"
	}
	n.Status.NodeInfo.OperatingSystem = os
	n.Status.NodeInfo.Architecture = "amd64"
	n.ObjectMeta.Labels["alpha.service-controller.kubernetes.io/exclude-balancer"] = "true"
}

// NodeConditions returns a list of conditions (Ready, OutOfDisk, etc), for updates to the node status
// within Kubernetes.
func (s *Provider) nodeConditions() []v1.NodeCondition {
	// TODO: Make this configurable
	return []v1.NodeCondition{
		{
			Type:               "Ready",
			Status:             v1.ConditionTrue,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletReady",
			Message:            "kubelet is ready.",
		},
		{
			Type:               "OutOfDisk",
			Status:             v1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasSufficientDisk",
			Message:            "kubelet has sufficient disk space available",
		},
		{
			Type:               "MemoryPressure",
			Status:             v1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasSufficientMemory",
			Message:            "kubelet has sufficient memory available",
		},
		{
			Type:               "DiskPressure",
			Status:             v1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasNoDiskPressure",
			Message:            "kubelet has no disk pressure",
		},
		{
			Type:               "NetworkUnavailable",
			Status:             v1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "RouteCreated",
			Message:            "RouteController created a route",
		},
	}
}
func buildKeyFromNames(namespace string, name string) (string, error) {
	return fmt.Sprintf("%s-%s", namespace, name), nil
}