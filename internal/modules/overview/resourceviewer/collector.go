package resourceviewer

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/heptio/developer-dash/internal/config"
	"github.com/heptio/developer-dash/internal/link"
	"github.com/heptio/developer-dash/internal/log"
	"github.com/heptio/developer-dash/internal/modules/overview/objectstatus"
	"github.com/heptio/developer-dash/internal/modules/overview/objectvisitor"
	"github.com/heptio/developer-dash/internal/objectstore"
	dashStrings "github.com/heptio/developer-dash/internal/util/strings"
	"github.com/heptio/developer-dash/pkg/view/component"
)

// CollectorOption is an option for configuring Collector.
type CollectorOption func(c *Collector)

// Collector collects objects to construct a resource viewer.
type Collector struct {
	edges  map[string][]string
	nodes  map[string]component.Node
	logger log.Logger

	// groupPods sets the pod grouping. If it is true, group pods in one
	// graph node. If not, show them separately.
	groupPods bool

	// podGroupIDs maps a pod to a pod group
	podGroupIDs map[string]string

	// podStats counts pods in a replica set.
	podStats map[string]int

	podNodes map[string]component.PodStatus

	objectStore objectstore.ObjectStore
	link        link.Interface

	mu          sync.Mutex
}

var _ objectvisitor.ObjectHandler = (*Collector)(nil)

// NewCollector creates an instance of Collector.
func NewCollector(dashConfig config.Dash, options ...CollectorOption) (*Collector, error) {
	l, err := link.NewFromDashConfig(dashConfig)
	if err != nil {
		return nil, err
	}

	collector := &Collector{
		podStats:    make(map[string]int),
		groupPods:   true,
		podGroupIDs: make(map[string]string),
		podNodes:    make(map[string]component.PodStatus),
		objectStore: dashConfig.ObjectStore(),
		link:        l,
	}

	for _, option := range options {
		option(collector)
	}

	collector.Reset()

	return collector, nil
}

// Reset resets a Collector's nodes and edges.
func (c *Collector) Reset() {
	c.edges = make(map[string][]string)
	c.nodes = make(map[string]component.Node)
}

// Process process an object by saving the object to a map.
func (c *Collector) Process(ctx context.Context, object objectvisitor.ClusterObject) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var uid string
	var node component.Node
	var err error

	if c.isPod(object) && c.groupPods {
		pod := &corev1.Pod{}
		if err := scheme.Scheme.Convert(object, pod, 0); err != nil {
			return errors.Wrap(err, "unable to convert object to pod")
		}

		if ownerReference := metav1.GetControllerOf(pod); ownerReference != nil {
			c.podStats[string(ownerReference.UID)]++

		}

		uid, node, err = c.createPodGroupNode(ctx, object)
	} else {
		uid, node, err = c.createObjectNode(ctx, object)
	}

	if err != nil {
		if isSkippedNode(err) {
			return nil
		}

		gvk := object.GetObjectKind().GroupVersionKind()
		accessor := meta.NewAccessor()
		name, err := accessor.Name(object)
		if err == nil {
			return errors.Wrapf(err, "processing unknown %s", gvk.String())
		}

		return errors.Wrapf(err, "processing %s %s", gvk.String(), name)
	}

	if _, ok := c.nodes[uid]; !ok {
		c.nodes[uid] = node
	}

	return nil
}

func (c *Collector) createPodGroupNode(ctx context.Context, object objectvisitor.ClusterObject) (string, component.Node, error) {
	pgd, err := c.podGroupDetails(object)
	if err != nil {
		return "", component.Node{}, errors.Wrap(err, "getting pod group id for pod")
	}

	accessor := meta.NewAccessor()
	uid, err := accessor.UID(object)
	if err != nil {
		return "", component.Node{}, errors.Wrap(err, "getting uid for pod")
	}

	name, err := accessor.Name(object)
	if err != nil {
		return "", component.Node{}, errors.Wrap(err, "getting name for pod")
	}

	status, err := objectstatus.Status(ctx, object, c.objectStore)
	if err != nil {
		return "", component.Node{}, errors.Wrap(err, "getting status for pod")
	}

	objectKind := object.GetObjectKind()
	apiVersion, kind := objectKind.GroupVersionKind().ToAPIVersionAndKind()

	podStatus, ok := c.podNodes[pgd.id]
	if !ok {
		podStatus = *component.NewPodStatus()
		c.podNodes[pgd.id] = podStatus
	}

	podStatus.AddSummary(name, status.Details, status.Status())

	node := component.Node{
		Name:       pgd.name,
		APIVersion: apiVersion,
		Kind:       kind,
		Status:     podStatus.Status(),
		Details: []component.Component{
			&podStatus,
		},
	}

	c.podGroupIDs[string(uid)] = pgd.id

	return pgd.id, node, nil
}

type isSkipped interface {
	IsSkipped() bool
}

func isSkippedNode(err error) bool {
	sn, ok := err.(isSkipped)
	return ok && sn.IsSkipped()
}

type skipNode struct{}

func (e skipNode) IsSkipped() bool {
	return true
}

func (e skipNode) Error() string {
	return "skip node"
}

func (c *Collector) createObjectNode(ctx context.Context, object objectvisitor.ClusterObject) (string, component.Node, error) {
	objectKind := object.GetObjectKind()
	gvk := objectKind.GroupVersionKind()
	apiVersion, kind := gvk.ToAPIVersionAndKind()

	accessor := meta.NewAccessor()

	if (gvk.Group == "apps" || gvk.Group == "extensions") &&
		gvk.Kind == "ReplicaSet" {
		apiVersion = "extensions/v1beta1"
		replicaSet := &appsv1.ReplicaSet{}
		if err := scheme.Scheme.Convert(object, replicaSet, nil); err != nil {
			return "", component.Node{}, errors.Wrap(err, "convert object to Replica Set")
		}

		replicas := replicaSet.Spec.Replicas
		if replicas == nil || *replicas < 1 {
			return "", component.Node{}, &skipNode{}
		}
	}

	uid, err := accessor.UID(object)
	if err != nil {
		return "", component.Node{}, err
	}

	name, err := accessor.Name(object)
	if err != nil {
		return "", component.Node{}, errors.New("unable to get object name")
	}

	var nodeStatus component.NodeStatus

	status, err := objectstatus.Status(ctx, object, c.objectStore)
	if err != nil {
		c.log().Errorf("error retrieving object status: %v", err)
		nodeStatus = component.NodeStatusOK
	} else {
		nodeStatus = status.Status()
	}

	q := url.Values{}
	objectPath, err := c.link.ForObjectWithQuery(object, name, q)
	if err != nil {
		return "", component.Node{}, err
	}

	node := component.Node{
		Name:       name,
		APIVersion: apiVersion,
		Kind:       kind,
		Status:     nodeStatus,
		Details:    status.Details,
		Path:       objectPath,
	}

	return string(uid), node, nil
}

// AddChild adds children for an object to create edges. Pods are collated to a single object.
func (c *Collector) AddChild(parent objectvisitor.ClusterObject, children ...objectvisitor.ClusterObject) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	accessor := meta.NewAccessor()
	uid, err := accessor.UID(parent)
	if err != nil {
		return err
	}

	pid := string(uid)

	for _, child := range children {
		var cid string

		if c.isPod(child) && c.groupPods {
			pgd, err := c.podGroupDetails(child)
			if err != nil {
				return errors.Wrap(err, "find pod group id for pod")
			}

			cid = pgd.id
		} else {
			id, err := accessor.UID(child)
			if err != nil {
				return err
			}

			cid = string(id)
		}

		if !dashStrings.Contains(cid, c.edges[pid]) {
			c.edges[pid] = append(c.edges[pid], cid)
		}
	}

	return nil
}

func (c *Collector) isPod(object objectvisitor.ClusterObject) bool {
	objectKind := object.GetObjectKind()
	gvk := objectKind.GroupVersionKind()

	return gvk.Group == "" &&
		gvk.Version == "v1" &&
		gvk.Kind == "Pod"
}

type podGroupDetails struct {
	id   string
	name string
}

func (c *Collector) podGroupDetails(object objectvisitor.ClusterObject) (podGroupDetails, error) {
	obj, err := meta.Accessor(object)
	if err != nil {
		return podGroupDetails{}, err
	}

	reference := metav1.GetControllerOf(obj)
	if reference == nil {

		return podGroupDetails{
			id:   string(obj.GetUID()),
			name: obj.GetName(),
		}, nil
	}

	id := fmt.Sprintf("pods-%s", reference.UID)

	pgd := podGroupDetails{
		id:   id,
		name: fmt.Sprintf("%s pods", reference.Name),
	}

	return pgd, nil
}

func (c *Collector) Component(selected string) (component.Component, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	nodes := make(map[string]component.Node)
	for k, v := range c.nodes {
		nodes[k] = v
	}

	rv := component.NewResourceViewer("Resource Viewer")

	var nodeIDs []string
	for nodeID, node := range nodes {
		if strings.HasPrefix(nodeID, "pods-") {
			ownerID := strings.TrimPrefix(nodeID, "pods-")
			node.Details = append(node.Details,
				component.NewText(fmt.Sprintf("Pod count: %d", c.podStats[ownerID])))
			nodes[nodeID] = node
		}

		rv.AddNode(nodeID, node)
		nodeIDs = append(nodeIDs, nodeID)
	}

	for nodeID, edges := range c.edges {
		sort.Strings(edges)
		for _, edgeID := range edges {
			if dashStrings.Contains(edgeID, nodeIDs) {
				rv.AddEdge(nodeID, edgeID, component.EdgeTypeExplicit)
			}
		}
	}

	podGroupID, ok := c.podGroupIDs[selected]
	if ok {
		selected = podGroupID
	}

	rv.Select(selected)

	return rv, nil
}

func (c *Collector) log() log.Logger {
	if c.logger != nil {
		return c.logger
	}

	return log.NopLogger()
}