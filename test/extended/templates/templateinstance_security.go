package templates

import (
	"fmt"
	"time"

	g "github.com/onsi/ginkgo"
	o "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	kapi "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/apis/storage"

	authorizationv1 "github.com/openshift/api/authorization/v1"
	templatev1 "github.com/openshift/api/template/v1"
	userv1 "github.com/openshift/api/user/v1"
	templatecontroller "github.com/openshift/openshift-controller-manager/pkg/template/controller"
	authorizationapi "github.com/openshift/origin/pkg/authorization/apis/authorization"
	routeapi "github.com/openshift/origin/pkg/route/apis/route"
	exutil "github.com/openshift/origin/test/extended/util"
)

// Check that objects created through the TemplateInstance mechanism are done
// impersonating the requester, and that privilege escalation is not possible.
var _ = g.Describe("[Conformance][templates] templateinstance security tests", func() {
	defer g.GinkgoRecover()

	var (
		cli = exutil.NewCLI("templates", exutil.KubeConfigPath())

		adminuser, edituser, editbygroupuser *userv1.User
		editgroup                            *userv1.Group

		dummyroute = &routeapi.Route{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "route",
				Namespace: "${NAMESPACE}",
			},
			Spec: routeapi.RouteSpec{
				To: routeapi.RouteTargetReference{
					Name: "dummyroute",
				},
			},
		}

		dummyrolebinding = &authorizationapi.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "rolebinding",
				Namespace: "${NAMESPACE}",
			},
			RoleRef: kapi.ObjectReference{
				Name: "admin",
			},
		}

		storageclass = &storage.StorageClass{
			ObjectMeta: metav1.ObjectMeta{
				Name: "storageclass",
			},
			Provisioner: "no-provisioning",
		}
	)

	g.Context("", func() {
		g.BeforeEach(func() {
			adminuser = createUser(cli, "adminuser", "admin")
			edituser = createUser(cli, "edituser", "edit")
			editbygroupuser = createUser(cli, "editbygroupuser", "")

			editgroup = createGroup(cli, "editgroup", "edit")
			addUserToGroup(cli, editbygroupuser.Name, editgroup.Name)

			// I think we get flakes when the group cache hasn't yet noticed the
			// new group membership made above.  Wait until all it looks like
			// all the users above have access to the namespace as expected.
			err := wait.PollImmediate(time.Second, 30*time.Second, func() (done bool, err error) {
				for _, user := range []*userv1.User{adminuser, edituser, editbygroupuser} {
					cli.ChangeUser(user.Name)
					sar, err := cli.AuthorizationClient().AuthorizationV1().LocalSubjectAccessReviews(cli.Namespace()).Create(&authorizationv1.LocalSubjectAccessReview{
						Action: authorizationv1.Action{
							Verb:     "get",
							Resource: "pods",
						},
					})
					if err != nil {
						return false, err
					}
					if !sar.Allowed {
						return false, nil
					}
				}
				return true, nil
			})
			o.Expect(err).NotTo(o.HaveOccurred())
		})

		g.AfterEach(func() {
			if g.CurrentGinkgoTestDescription().Failed {
				templateinstances, err := cli.AdminTemplateClient().TemplateV1().TemplateInstances(cli.Namespace()).List(metav1.ListOptions{})
				if err == nil {
					fmt.Fprintf(g.GinkgoWriter, "TemplateInstances: %#v", templateinstances.Items)
				}
			}

			deleteUser(cli, adminuser)
			deleteUser(cli, edituser)
			deleteUser(cli, editbygroupuser)

			deleteGroup(cli, editgroup)
		})

		g.It("should pass security tests", func() {
			tests := []struct {
				by              string
				user            *userv1.User
				namespace       string
				objects         []runtime.Object
				expectCondition templatev1.TemplateInstanceConditionType
				checkOK         func(namespace string) bool
			}{
				{
					by:              "checking edituser can create an object in a permitted namespace",
					user:            edituser,
					namespace:       cli.Namespace(),
					objects:         []runtime.Object{dummyroute},
					expectCondition: templatev1.TemplateInstanceReady,
					checkOK: func(namespace string) bool {
						_, err := cli.AdminRouteClient().RouteV1().Routes(namespace).Get(dummyroute.Name, metav1.GetOptions{})
						return err == nil
					},
				},
				{
					by:              "checking editbygroupuser can create an object in a permitted namespace",
					user:            editbygroupuser,
					namespace:       cli.Namespace(),
					objects:         []runtime.Object{dummyroute},
					expectCondition: templatev1.TemplateInstanceReady,
					checkOK: func(namespace string) bool {
						_, err := cli.AdminRouteClient().RouteV1().Routes(namespace).Get(dummyroute.Name, metav1.GetOptions{})
						return err == nil
					},
				},
				{
					by:              "checking edituser can't create an object in a non-permitted namespace",
					user:            edituser,
					namespace:       "default",
					objects:         []runtime.Object{dummyroute},
					expectCondition: templatev1.TemplateInstanceInstantiateFailure,
					checkOK: func(namespace string) bool {
						_, err := cli.AdminRouteClient().RouteV1().Routes(namespace).Get(dummyroute.Name, metav1.GetOptions{})
						return err != nil && kerrors.IsNotFound(err)
					},
				},
				{
					by:              "checking editbygroupuser can't create an object in a non-permitted namespace",
					user:            editbygroupuser,
					namespace:       "default",
					objects:         []runtime.Object{dummyroute},
					expectCondition: templatev1.TemplateInstanceInstantiateFailure,
					checkOK: func(namespace string) bool {
						_, err := cli.AdminRouteClient().RouteV1().Routes(namespace).Get(dummyroute.Name, metav1.GetOptions{})
						return err != nil && kerrors.IsNotFound(err)
					},
				},
				{
					by:              "checking edituser can't create an object that requires admin",
					user:            edituser,
					namespace:       cli.Namespace(),
					objects:         []runtime.Object{dummyrolebinding},
					expectCondition: templatev1.TemplateInstanceInstantiateFailure,
					checkOK: func(namespace string) bool {
						_, err := cli.AdminAuthorizationClient().AuthorizationV1().RoleBindings(namespace).Get(dummyrolebinding.Name, metav1.GetOptions{})
						return err != nil && kerrors.IsNotFound(err)
					},
				},
				{
					by:              "checking editbygroupuser can't create an object that requires admin",
					user:            editbygroupuser,
					namespace:       cli.Namespace(),
					objects:         []runtime.Object{dummyrolebinding},
					expectCondition: templatev1.TemplateInstanceInstantiateFailure,
					checkOK: func(namespace string) bool {
						_, err := cli.AdminAuthorizationClient().AuthorizationV1().RoleBindings(namespace).Get(dummyrolebinding.Name, metav1.GetOptions{})
						return err != nil && kerrors.IsNotFound(err)
					},
				},
				{
					by:              "checking adminuser can create an object that requires admin",
					user:            adminuser,
					namespace:       cli.Namespace(),
					objects:         []runtime.Object{dummyrolebinding},
					expectCondition: templatev1.TemplateInstanceReady,
					checkOK: func(namespace string) bool {
						_, err := cli.AdminAuthorizationClient().AuthorizationV1().RoleBindings(namespace).Get(dummyrolebinding.Name, metav1.GetOptions{})
						return err == nil
					},
				},
				{
					by:              "checking adminuser can't create an object that requires more than admin",
					user:            adminuser,
					namespace:       cli.Namespace(),
					objects:         []runtime.Object{storageclass},
					expectCondition: templatev1.TemplateInstanceInstantiateFailure,
					checkOK: func(namespace string) bool {
						_, err := cli.AdminKubeClient().StorageV1().StorageClasses().Get(storageclass.Name, metav1.GetOptions{})
						return err != nil && kerrors.IsNotFound(err)
					},
				},
			}

			targetVersions := []schema.GroupVersion{storagev1.SchemeGroupVersion}
			targetVersions = append(targetVersions, legacyscheme.Scheme.PrioritizedVersionsAllGroups()...)

			for _, test := range tests {
				g.By(test.by)
				cli.ChangeUser(test.user.Name)

				secret, err := cli.KubeClient().CoreV1().Secrets(cli.Namespace()).Create(&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name: "secret",
					},
					Data: map[string][]byte{
						"NAMESPACE": []byte(test.namespace),
					},
				})
				o.Expect(err).NotTo(o.HaveOccurred())

				templateinstance := &templatev1.TemplateInstance{
					ObjectMeta: metav1.ObjectMeta{
						Name: "templateinstance",
					},
					Spec: templatev1.TemplateInstanceSpec{
						Template: templatev1.Template{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "template",
								Namespace: cli.Namespace(),
							},
							Parameters: []templatev1.Parameter{
								{
									Name:     "NAMESPACE",
									Required: true,
								},
							},
						},
						Secret: &corev1.LocalObjectReference{
							Name: "secret",
						},
					},
				}

				for i := range test.objects {
					templateinstance.Spec.Template.Objects = append(templateinstance.Spec.Template.Objects, runtime.RawExtension{
						Raw: []byte(runtime.EncodeOrDie(legacyscheme.Codecs.LegacyCodec(legacyscheme.Scheme.PrioritizedVersionsAllGroups()...), test.objects[i])),
					})
				}

				templateinstance, err = cli.TemplateClient().TemplateV1().TemplateInstances(cli.Namespace()).Create(templateinstance)
				o.Expect(err).NotTo(o.HaveOccurred())

				err = wait.Poll(100*time.Millisecond, 1*time.Minute, func() (bool, error) {
					templateinstance, err = cli.TemplateClient().TemplateV1().TemplateInstances(cli.Namespace()).Get(templateinstance.Name, metav1.GetOptions{})
					if err != nil {
						return false, err
					}
					return len(templateinstance.Status.Conditions) != 0, nil
				})
				o.Expect(err).NotTo(o.HaveOccurred())

				o.Expect(templatecontroller.TemplateInstanceHasCondition(templateinstance, test.expectCondition, corev1.ConditionTrue)).To(o.Equal(true))
				o.Expect(test.checkOK(test.namespace)).To(o.BeTrue())

				foreground := metav1.DeletePropagationForeground
				err = cli.TemplateClient().TemplateV1().TemplateInstances(cli.Namespace()).Delete(templateinstance.Name, &metav1.DeleteOptions{PropagationPolicy: &foreground})
				o.Expect(err).NotTo(o.HaveOccurred())

				// wait for garbage collector to do its thing
				err = wait.Poll(100*time.Millisecond, 30*time.Second, func() (bool, error) {
					_, err = cli.TemplateClient().TemplateV1().TemplateInstances(cli.Namespace()).Get(templateinstance.Name, metav1.GetOptions{})
					if kerrors.IsNotFound(err) {
						return true, nil
					}
					return false, err
				})
				o.Expect(err).NotTo(o.HaveOccurred())

				err = cli.KubeClient().CoreV1().Secrets(cli.Namespace()).Delete(secret.Name, nil)
				o.Expect(err).NotTo(o.HaveOccurred())
			}
		})
	})
})
