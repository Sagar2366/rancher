package globaldns

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strings"

	access "github.com/rancher/rancher/pkg/api/customization/globalnamespaceaccess"
	"github.com/rancher/rancher/pkg/controllers/management/globalnamespacerbac"
	"github.com/rancher/rancher/pkg/namespace"
	"github.com/rancher/types/apis/management.cattle.io/v3"
	"github.com/rancher/types/config"
	"github.com/sirupsen/logrus"

	"github.com/rancher/types/client/management/v3"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/api/extensions/v1beta1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientv1beta1 "k8s.io/client-go/kubernetes/typed/extensions/v1beta1"
)

const (
	GlobaldnsController    = "mgmt-global-dns-controller"
	annotationIngressClass = "kubernetes.io/ingress.class"
)

type GDController struct {
	globalDNSs        v3.GlobalDNSInterface
	globalDNSLister   v3.GlobalDNSLister
	ingresses         clientv1beta1.IngressInterface //need to use client-go IngressInterface to update Ingress.Status field
	managementContext *config.ManagementContext
	prtbLister        v3.ProjectRoleTemplateBindingLister
	rtLister          v3.RoleTemplateLister
}

func newGlobalDNSController(ctx context.Context, mgmt *config.ManagementContext) *GDController {
	n := &GDController{
		globalDNSs:        mgmt.Management.GlobalDNSs(namespace.GlobalNamespace),
		globalDNSLister:   mgmt.Management.GlobalDNSs(namespace.GlobalNamespace).Controller().Lister(),
		ingresses:         mgmt.K8sClient.Extensions().Ingresses(namespace.GlobalNamespace),
		managementContext: mgmt,
		prtbLister:        mgmt.Management.ProjectRoleTemplateBindings("").Controller().Lister(),
		rtLister:          mgmt.Management.RoleTemplates("").Controller().Lister(),
	}
	return n
}

//sync is called periodically and on real updates
func (n *GDController) sync(key string, obj *v3.GlobalDNS) (runtime.Object, error) {
	if obj == nil || obj.DeletionTimestamp != nil {
		return nil, nil
	}

	metaAccessor, err := meta.Accessor(obj)
	if err != nil {
		return nil, err
	}
	creatorID, ok := metaAccessor.GetAnnotations()[globalnamespacerbac.CreatorIDAnn]
	if !ok {
		return nil, fmt.Errorf("GlobalDNS %v has no creatorId annotation", metaAccessor.GetName())
	}

	//check if status.endpoints is set, if yes create a dummy ingress if not already present
	//if ingress exists, update endpoints if different

	var isUpdate bool

	//check if ingress for this globaldns is already present
	ingress, err := n.getIngressForGlobalDNS(obj)

	if err != nil && !k8serrors.IsNotFound(err) {
		return nil, fmt.Errorf("GlobalDNSController: Error listing ingress for the GlobalDNS %v", err)
	}

	if ingress != nil && err == nil {
		isUpdate = true
	}

	if len(obj.Status.Endpoints) == 0 && !isUpdate {
		return nil, nil
	}

	if !isUpdate {
		ingress, err = n.createIngressForGlobalDNS(obj)
		if err != nil {
			return nil, fmt.Errorf("GlobalDNSController: Error creating an ingress for the GlobalDNS %v", err)
		}
	}

	err = n.updateIngressEndpoints(ingress, obj.Status.Endpoints)
	if err != nil {
		return nil, fmt.Errorf("GlobalDNSController: Error updating ingress for the GlobalDNS %v", err)
	}

	groups := globalnamespacerbac.GetMemberGroups(obj.Spec.Members)
	if err := access.CheckGroupAccess(groups, obj.Spec.ProjectNames, n.prtbLister, n.rtLister, "", client.GlobalDNSType); err != nil {
		return nil, err
	}

	currentMembers := globalnamespacerbac.GetCurrentMembers(obj.Spec.Members)
	updatedMembers, err := globalnamespacerbac.GetUpdatedMembers(obj.Spec.ProjectNames, obj.Spec.Members, n.prtbLister)
	if err := globalnamespacerbac.CreateRoleAndRoleBinding(globalnamespacerbac.GlobalDNSResource, obj.Name, updatedMembers, creatorID, n.managementContext); err != nil {
		return nil, err
	}

	if !reflect.DeepEqual(updatedMembers, currentMembers) {
		toUpdate := obj.DeepCopy()
		toUpdate.Spec.Members = updatedMembers
		_, err := n.globalDNSs.Update(toUpdate)
		if err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func (n *GDController) getIngressForGlobalDNS(globaldns *v3.GlobalDNS) (*v1beta1.Ingress, error) {
	ingress, err := n.ingresses.Get(strings.Join([]string{"globaldns-ingress", globaldns.Name}, "-"), metav1.GetOptions{}) //n.Get("", strings.Join([]string{"globaldns-ingress", globaldns.Name}, "-"))
	if err != nil {
		return nil, err
	}
	//make sure the ingress is owned by this globalDNS
	if n.isIngressOwnedByGlobalDNS(ingress, globaldns) {
		return ingress, nil
	}
	return nil, nil
}

func (n *GDController) isIngressOwnedByGlobalDNS(ingress *v1beta1.Ingress, globaldns *v3.GlobalDNS) bool {
	for i, owners := 0, ingress.GetOwnerReferences(); owners != nil && i < len(owners); i++ {
		if owners[i].UID == globaldns.UID && owners[i].Kind == globaldns.Kind {
			return true
		}
	}
	return false
}

func (n *GDController) createIngressForGlobalDNS(globaldns *v3.GlobalDNS) (*v1beta1.Ingress, error) {
	ingressSpec := n.generateNewIngressSpec(globaldns)
	ingressObj, err := n.ingresses.Create(ingressSpec)
	if err != nil {
		return nil, err
	}
	logrus.Debugf("Created ingress %v for globalDNS %s", ingressObj.Name, globaldns.Name)
	return ingressObj, nil
}

func (n *GDController) generateNewIngressSpec(globaldns *v3.GlobalDNS) *v1beta1.Ingress {
	controller := true
	return &v1beta1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name: strings.Join([]string{"globaldns-ingress", globaldns.Name}, "-"),
			OwnerReferences: []metav1.OwnerReference{
				{
					Name:       globaldns.Name,
					APIVersion: "v3",
					UID:        globaldns.UID,
					Kind:       globaldns.Kind,
					Controller: &controller,
				},
			},
			Annotations: map[string]string{
				annotationIngressClass: "rancher-external-dns",
			},
			Namespace: globaldns.Namespace,
		},
		Spec: v1beta1.IngressSpec{
			Rules: []v1beta1.IngressRule{
				{
					Host: globaldns.Spec.FQDN,
					IngressRuleValue: v1beta1.IngressRuleValue{
						HTTP: &v1beta1.HTTPIngressRuleValue{
							Paths: []v1beta1.HTTPIngressPath{
								{
									Backend: v1beta1.IngressBackend{
										ServiceName: "http-svc-dummy",
										ServicePort: intstr.IntOrString{
											Type:   intstr.Int,
											IntVal: 42,
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func (n *GDController) updateIngressEndpoints(ingress *v1beta1.Ingress, endpoints []string) error {

	if !n.ifEndpointsDiffer(ingress.Status.LoadBalancer.Ingress, endpoints) {
		return nil
	}

	ingress.Status.LoadBalancer.Ingress = n.sliceToStatus(endpoints)
	updatedObj, err := n.ingresses.UpdateStatus(ingress)

	if err != nil {
		return fmt.Errorf("GlobalDNSController: Error updating Ingress %v", err)
	}

	logrus.Debugf("GlobalDNSController: Updated Ingress: %v", updatedObj)
	return nil
}

func (n *GDController) ifEndpointsDiffer(ingressEps []apiv1.LoadBalancerIngress, endpoints []string) bool {
	if len(ingressEps) != len(endpoints) {
		return true
	}

	mapIngEndpoints := n.gatherIngressEndpoints(ingressEps)
	for _, ep := range endpoints {
		if !mapIngEndpoints[ep] {
			return true
		}
	}
	return false
}

func (n *GDController) gatherIngressEndpoints(ingressEps []apiv1.LoadBalancerIngress) map[string]bool {
	mapIngEndpoints := make(map[string]bool)
	for _, ep := range ingressEps {
		if ep.IP != "" {
			mapIngEndpoints[ep.IP] = true
		} else if ep.Hostname != "" {
			mapIngEndpoints[ep.Hostname] = true
		}
	}
	return mapIngEndpoints
}

// sliceToStatus converts a slice of IP and/or hostnames to LoadBalancerIngress
func (n *GDController) sliceToStatus(endpoints []string) []apiv1.LoadBalancerIngress {
	lbi := []apiv1.LoadBalancerIngress{}
	for _, ep := range endpoints {
		if net.ParseIP(ep) == nil {
			lbi = append(lbi, apiv1.LoadBalancerIngress{Hostname: ep})
		} else {
			lbi = append(lbi, apiv1.LoadBalancerIngress{IP: ep})
		}
	}
	return lbi
}
