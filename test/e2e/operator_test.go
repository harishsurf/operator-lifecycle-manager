package e2e

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	gomegatypes "github.com/onsi/gomega/types"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/reference"
	controllerclient "sigs.k8s.io/controller-runtime/pkg/client"

	operatorsv1 "github.com/operator-framework/api/pkg/operators/v1"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	operatorsv2alpha1 "github.com/operator-framework/api/pkg/operators/v2alpha1"
	clientv2alpha1 "github.com/operator-framework/operator-lifecycle-manager/pkg/api/client/clientset/versioned/typed/operators/v2alpha1"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/controller/operators/decorators"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/lib/testobj"
	"github.com/operator-framework/operator-lifecycle-manager/test/e2e/ctx"
)

// Describes test specs for the Operator resource.
var _ = Describe("Operator", func() {
	var (
		clientCtx       context.Context
		scheme          *runtime.Scheme
		listOpts        metav1.ListOptions
		operatorClient  clientv2alpha1.OperatorInterface
		client          controllerclient.Client
		operatorFactory decorators.OperatorFactory
	)

	BeforeEach(func() {
		// Toggle v2alpha1 feature-gate
		toggleCVO()
		togglev2alpha1()

		// Setup common utilities
		clientCtx = context.Background()
		scheme = ctx.Ctx().Scheme()
		listOpts = metav1.ListOptions{}
		operatorClient = ctx.Ctx().OperatorClient().OperatorsV2alpha1().Operators()
		client = ctx.Ctx().Client()

		var err error
		operatorFactory, err = decorators.NewSchemedOperatorFactory(scheme)
		Expect(err).ToNot(HaveOccurred())
	})

	AfterEach(func() {
		togglev2alpha1()
		toggleCVO()
	})

	// Ensures that an Operator resource can select its components by label and surface them correctly in its status.
	//
	// Steps:
	// 1. Create an Operator resource, o
	// 2. Ensure o's status eventually contains its component label selector
	// 3. Create namespaces ns-a and ns-b
	// 4. Label ns-a with o's component label
	// 5. Ensure o's status.components.refs field eventually contains a reference to ns-a
	// 6. Create ServiceAccounts sa-a and sa-b in namespaces ns-a and ns-b respectively
	// 7. Label sa-a and sa-b with o's component label
	// 8. Ensure o's status.components.refs field eventually contains references to sa-a and sa-b
	// 9. Remove the component label from sa-b
	// 10. Ensure the reference to sa-b is eventually removed from o's status.components.refs field
	// 11. Delete ns-a
	// 12. Ensure the reference to ns-a is eventually removed from o's status.components.refs field
	It("should surface components in its status", func() {
		o := &operatorsv2alpha1.Operator{}
		o.SetName(genName("o-"))

		Eventually(func() error {
			return client.Create(clientCtx, o)
		}).Should(Succeed())

		defer func() {
			Eventually(func() error {
				err := client.Delete(clientCtx, o)
				if apierrors.IsNotFound(err) {
					return nil
				}

				return err
			}).Should(Succeed())
		}()

		By("eventually having a status that contains its component label selector")
		w, err := operatorClient.Watch(clientCtx, listOpts)
		Expect(err).ToNot(HaveOccurred())
		defer w.Stop()

		deadline, cancel := context.WithTimeout(clientCtx, 1*time.Minute)
		defer cancel()

		expectedKey := "operators.coreos.com/" + o.GetName()
		awaitPredicates(deadline, w, operatorPredicate(func(op *operatorsv2alpha1.Operator) bool {
			if op.Status.Components == nil || op.Status.Components.LabelSelector == nil {
				return false
			}

			for _, requirement := range op.Status.Components.LabelSelector.MatchExpressions {
				if requirement.Key == expectedKey && requirement.Operator == metav1.LabelSelectorOpExists {
					return true
				}
			}

			return false
		}))
		defer w.Stop()

		// Create namespaces ns-a and ns-b
		nsA := &corev1.Namespace{}
		nsA.SetName(genName("ns-a-"))
		nsB := &corev1.Namespace{}
		nsB.SetName(genName("ns-b-"))

		for _, ns := range []*corev1.Namespace{nsA, nsB} {
			Eventually(func() error {
				return client.Create(clientCtx, ns)
			}).Should(Succeed())

			defer func(n *corev1.Namespace) {
				Eventually(func() error {
					err := client.Delete(clientCtx, n)
					if apierrors.IsNotFound(err) {
						return nil
					}
					return err
				}).Should(Succeed())
			}(ns)
		}

		// Label ns-a with o's component label
		setComponentLabel := func(m metav1.Object) error {
			m.SetLabels(map[string]string{expectedKey: ""})
			return nil
		}
		Eventually(Apply(nsA, setComponentLabel)).Should(Succeed())

		// Ensure o's status.components.refs field eventually contains a reference to ns-a
		By("eventually listing a single component reference")
		componentRefEventuallyExists(w, true, getReference(scheme, nsA))

		// Create ServiceAccounts sa-a and sa-b in namespaces ns-a and ns-b respectively
		saA := &corev1.ServiceAccount{}
		saA.SetName(genName("sa-a-"))
		saA.SetNamespace(nsA.GetName())
		saB := &corev1.ServiceAccount{}
		saB.SetName(genName("sa-b-"))
		saB.SetNamespace(nsB.GetName())

		for _, sa := range []*corev1.ServiceAccount{saA, saB} {
			Eventually(func() error {
				return client.Create(clientCtx, sa)
			}).Should(Succeed())
			defer func(sa *corev1.ServiceAccount) {
				Eventually(func() error {
					err := client.Delete(clientCtx, sa)
					if apierrors.IsNotFound(err) {
						return nil
					}
					return err
				}).Should(Succeed())
			}(sa)
		}

		// Label sa-a and sa-b with o's component label
		Eventually(Apply(saA, setComponentLabel)).Should(Succeed())
		Eventually(Apply(saB, setComponentLabel)).Should(Succeed())

		// Ensure o's status.components.refs field eventually contains references to sa-a and sa-b
		By("eventually listing multiple component references")
		componentRefEventuallyExists(w, true, getReference(scheme, saA))
		componentRefEventuallyExists(w, true, getReference(scheme, saB))

		// Remove the component label from sa-b
		Eventually(Apply(saB, func(m metav1.Object) error {
			m.SetLabels(nil)
			return nil
		})).Should(Succeed())

		// Ensure the reference to sa-b is eventually removed from o's status.components.refs field
		By("removing a component's reference when it no longer bears the component label")
		componentRefEventuallyExists(w, false, getReference(scheme, saB))

		// Delete ns-a
		Eventually(func() error {
			err := client.Delete(clientCtx, nsA)
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}).Should(Succeed())

		// Ensure the reference to ns-a is eventually removed from o's status.components.refs field
		By("removing a component's reference when it no longer exists")
		componentRefEventuallyExists(w, false, getReference(scheme, nsA))
	})

	Context("when a subscription to a package exists", func() {
		var (
			ns           *corev1.Namespace
			sub          *operatorsv1alpha1.Subscription
			operatorName types.NamespacedName
		)

		BeforeEach(func() {
			// Subscribe to a package and await a successful install
			ns = &corev1.Namespace{}
			ns.SetName(genName("ns-"))
			Eventually(func() error {
				return client.Create(clientCtx, ns)
			}).Should(Succeed())

			// Default to AllNamespaces
			og := &operatorsv1.OperatorGroup{}
			og.SetNamespace(ns.GetName())
			og.SetName(genName("og-"))
			Eventually(func() error {
				return client.Create(clientCtx, og)
			}).Should(Succeed())

			cs := &operatorsv1alpha1.CatalogSource{
				Spec: operatorsv1alpha1.CatalogSourceSpec{
					SourceType: operatorsv1alpha1.SourceTypeGrpc,
					Image:      "quay.io/olmtest/single-bundle-index:1.0.0",
				},
			}
			cs.SetNamespace(ns.GetName())
			cs.SetName(genName("cs-"))
			Eventually(func() error {
				return client.Create(clientCtx, cs)
			}).Should(Succeed())

			sub = &operatorsv1alpha1.Subscription{
				Spec: &operatorsv1alpha1.SubscriptionSpec{
					CatalogSource:          cs.GetName(),
					CatalogSourceNamespace: cs.GetNamespace(),
					Package:                "kiali",
					Channel:                "stable",
					InstallPlanApproval:    operatorsv1alpha1.ApprovalAutomatic,
				},
			}
			sub.SetNamespace(cs.GetNamespace())
			sub.SetName(genName("sub-"))
			Eventually(func() error {
				return client.Create(clientCtx, sub)
			}).Should(Succeed())

			Eventually(func() (operatorsv1alpha1.SubscriptionState, error) {
				s := sub.DeepCopy()
				if err := client.Get(clientCtx, testobj.NamespacedName(s), s); err != nil {
					return operatorsv1alpha1.SubscriptionStateNone, err
				}

				return s.Status.State, nil
			}).Should(BeEquivalentTo(operatorsv1alpha1.SubscriptionStateAtLatest))

			operator, err := operatorFactory.NewPackageOperator(sub.Spec.Package, sub.GetNamespace())
			Expect(err).ToNot(HaveOccurred())
			operatorName = testobj.NamespacedName(operator)
		})

		AfterEach(func() {
			Eventually(func() error {
				err := client.Delete(clientCtx, ns)
				if apierrors.IsNotFound(err) {
					return nil
				}
				return err
			}).Should(Succeed())
		})

		It("should automatically adopt components", func() {
			Eventually(func() (*operatorsv2alpha1.Operator, error) {
				o := &operatorsv2alpha1.Operator{}
				err := client.Get(clientCtx, operatorName, o)
				return o, err
			}).Should(ReferenceComponents([]*corev1.ObjectReference{
				getReference(scheme, sub),
				getReference(scheme, testobj.WithNamespacedName(
					&types.NamespacedName{Namespace: sub.GetNamespace(), Name: "kiali-operator.v1.4.2"},
					&operatorsv1alpha1.ClusterServiceVersion{},
				)),
				getReference(scheme, testobj.WithNamespacedName(
					&types.NamespacedName{Namespace: sub.GetNamespace(), Name: "kiali-operator"},
					&corev1.ServiceAccount{},
				)),
				getReference(scheme, testobj.WithName("kialis.kiali.io", &apiextensionsv1.CustomResourceDefinition{})),
				getReference(scheme, testobj.WithName("monitoringdashboards.monitoring.kiali.io", &apiextensionsv1.CustomResourceDefinition{})),
			}))
		})
	})

})

func getReference(scheme *runtime.Scheme, obj runtime.Object) *corev1.ObjectReference {
	ref, err := reference.GetReference(scheme, obj)
	if err != nil {
		panic(fmt.Sprintf("unable to get object reference: %s", err))
	}
	ref.UID = ""
	ref.ResourceVersion = ""

	return ref
}

func componentRefEventuallyExists(w watch.Interface, exists bool, ref *corev1.ObjectReference) {
	deadline, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()

	awaitPredicates(deadline, w, operatorPredicate(func(op *operatorsv2alpha1.Operator) bool {
		if op.Status.Components == nil {
			return false
		}

		for _, r := range op.Status.Components.Refs {
			if r.APIVersion == ref.APIVersion && r.Kind == ref.Kind && r.Namespace == ref.Namespace && r.Name == ref.Name {
				return exists
			}
		}

		return !exists
	}))
}

func operatorPredicate(fn func(*operatorsv2alpha1.Operator) bool) predicateFunc {
	return func(event watch.Event) bool {
		o, ok := event.Object.(*operatorsv2alpha1.Operator)
		if !ok {
			panic(fmt.Sprintf("unexpected event object type %T in deployment", event.Object))
		}

		return fn(o)
	}
}

type OperatorMatcher struct {
	matches func(*operatorsv2alpha1.Operator) (bool, error)
	name    string
}

func (o OperatorMatcher) Match(actual interface{}) (bool, error) {
	operator, ok := actual.(*operatorsv2alpha1.Operator)
	if !ok {
		return false, fmt.Errorf("OperatorMatcher expects Operator (got %T)", actual)
	}

	return o.matches(operator)
}

func (o OperatorMatcher) String() string {
	return o.name
}

func (o OperatorMatcher) FailureMessage(actual interface{}) string {
	return format.Message(actual, "to satisfy", o)
}

func (o OperatorMatcher) NegatedFailureMessage(actual interface{}) string {
	return format.Message(actual, "not to satisfy", o)
}

func ReferenceComponents(refs []*corev1.ObjectReference) gomegatypes.GomegaMatcher {
	return &OperatorMatcher{
		matches: func(operator *operatorsv2alpha1.Operator) (bool, error) {
			actual := map[corev1.ObjectReference]struct{}{}
			for _, ref := range operator.Status.Components.Refs {
				actual[*ref.ObjectReference] = struct{}{}
			}

			for _, ref := range refs {
				if _, ok := actual[*ref]; !ok {
					return false, nil
				}
			}

			return true, nil
		},
		name: fmt.Sprintf("ReferenceComponents(%v)", refs),
	}
}
