package imagepolicy

import (
	"bytes"
	"io/ioutil"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/diff"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/admission"
	corev1listers "k8s.io/client-go/listers/core/v1"
	clientgotesting "k8s.io/client-go/testing"
	kcache "k8s.io/client-go/tools/cache"
	"k8s.io/kubernetes/pkg/apis/apps"
	kapi "k8s.io/kubernetes/pkg/apis/core"

	"github.com/openshift/api/image"
	imagev1 "github.com/openshift/api/image/v1"
	imagev1fakeclient "github.com/openshift/client-go/image/clientset/versioned/fake"
	"github.com/openshift/library-go/pkg/image/reference"
	buildapi "github.com/openshift/origin/pkg/build/apis/build"
	imagepolicy "github.com/openshift/origin/pkg/image/apiserver/admission/apis/imagepolicy/v1"
	"github.com/openshift/origin/pkg/image/apiserver/admission/apis/imagepolicy/validation"
	"github.com/openshift/origin/pkg/image/apiserver/admission/imagepolicy/rules"
)

const (
	goodSHA = "sha256:08151bf2fc92355f236918bb16905921e6f66e1d03100fb9b18d60125db3df3a"
	badSHA  = "sha256:503c75e8121369581e5e5abe57b5a3f12db859052b217a8ea16eb86f4b5561a1"
)

var (
	buildGroupVersionResource = schema.GroupVersionResource{Group: "build.openshift.io", Version: "v1", Resource: "builds"}
	buildGroupVersionKind     = schema.GroupVersionKind{Group: "build.openshift.io", Version: "v1", Kind: "Build"}

	buildConfigGroupVersionResource = schema.GroupVersionResource{Group: "build.openshift.io", Version: "v1", Resource: "buildconfigs"}
	buildConfigGroupVersionKind     = schema.GroupVersionKind{Group: "build.openshift.io", Version: "v1", Kind: "BuildConfig"}
)

type resolveFunc func(ref *kapi.ObjectReference, defaultNamespace string, forceLocalResolve bool) (*rules.ImagePolicyAttributes, error)

func (fn resolveFunc) ResolveObjectReference(ref *kapi.ObjectReference, defaultNamespace string, forceLocalResolve bool) (*rules.ImagePolicyAttributes, error) {
	return fn(ref, defaultNamespace, forceLocalResolve)
}

func setDefaultCache(p *imagePolicyPlugin) kcache.Indexer {
	indexer := kcache.NewIndexer(kcache.MetaNamespaceKeyFunc, kcache.Indexers{})
	p.nsLister = corev1listers.NewNamespaceLister(indexer)
	return indexer
}

func TestDefaultPolicy(t *testing.T) {
	input, err := os.Open("../apis/imagepolicy/v1/default-policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	config := &imagepolicy.ImagePolicyConfig{}
	configContent, err := ioutil.ReadAll(input)
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	utilruntime.Must(imagepolicy.Install(scheme))
	codecs := serializer.NewCodecFactory(scheme)
	err = runtime.DecodeInto(codecs.UniversalDecoder(imagepolicy.GroupVersion), configContent, config)
	if err != nil {
		t.Fatal(err)
	}
	imagepolicy.SetDefaults_ImagePolicyConfig(config)

	if errs := validation.Validate(config); len(errs) > 0 {
		t.Fatal(errs.ToAggregate())
	}

	plugin, err := newImagePolicyPlugin(config)
	if err != nil {
		t.Fatal(err)
	}

	goodImage := &imagev1.Image{
		ObjectMeta:           metav1.ObjectMeta{Name: goodSHA},
		DockerImageReference: "integrated.registry/goodns/goodimage:good",
	}
	badImage := &imagev1.Image{
		ObjectMeta: metav1.ObjectMeta{
			Name: badSHA,
			Annotations: map[string]string{
				"images.openshift.io/deny-execution": "true",
			},
		},
		DockerImageReference: "integrated.registry/badns/badimage:bad",
	}

	goodTag := &imagev1.ImageStreamTag{
		ObjectMeta: metav1.ObjectMeta{Name: "mysql:goodtag", Namespace: "repo"},
		Image:      *goodImage,
	}
	badTag := &imagev1.ImageStreamTag{
		ObjectMeta: metav1.ObjectMeta{Name: "mysql:badtag", Namespace: "repo"},
		Image:      *badImage,
	}

	client := &imagev1fakeclient.Clientset{}
	client.AddReactor("get", "images", func(action clientgotesting.Action) (handled bool, ret runtime.Object, err error) {
		name := action.(clientgotesting.GetAction).GetName()
		switch name {
		case goodImage.Name:
			return true, goodImage, nil
		case badImage.Name:
			return true, badImage, nil
		default:
			return true, nil, kerrors.NewNotFound(image.Resource("images"), name)
		}
	})
	client.AddReactor("get", "imagestreamtags", func(action clientgotesting.Action) (handled bool, ret runtime.Object, err error) {
		name := action.(clientgotesting.GetAction).GetName()
		switch name {
		case goodTag.Name:
			return true, goodTag, nil
		case badTag.Name:
			return true, badTag, nil
		default:
			return true, nil, kerrors.NewNotFound(image.Resource("imagestreamtags"), name)
		}
	})

	store := setDefaultCache(plugin)
	plugin.client = client
	plugin.SetDefaultRegistryFunc(func() (string, bool) {
		return "integrated.registry", true
	})
	if err := plugin.ValidateInitialization(); err != nil {
		t.Fatal(err)
	}

	originalNowFn := now
	defer (func() { now = originalNowFn })()
	now = func() time.Time { return time.Unix(1, 0) }

	// should allow a non-integrated image
	attrs := admission.NewAttributesRecord(
		&kapi.Pod{Spec: kapi.PodSpec{Containers: []kapi.Container{{Image: "index.docker.io/mysql:latest"}}}},
		nil, schema.GroupVersionKind{Version: "v1", Kind: "Pod"},
		"default", "pod1", schema.GroupVersionResource{Version: "v1", Resource: "pods"},
		"", admission.Create, false, nil,
	)
	if err := plugin.Admit(attrs, nil); err != nil {
		t.Fatal(err)
	}
	if err := plugin.Validate(attrs, nil); err != nil {
		t.Fatal(err)
	}

	// should resolve the non-integrated image and allow it
	attrs = admission.NewAttributesRecord(
		&kapi.Pod{Spec: kapi.PodSpec{Containers: []kapi.Container{{Image: "index.docker.io/mysql@" + goodSHA}}}},
		nil, schema.GroupVersionKind{Version: "v1", Kind: "Pod"},
		"default", "pod1", schema.GroupVersionResource{Version: "v1", Resource: "pods"},
		"", admission.Create, false, nil,
	)
	if err := plugin.Admit(attrs, nil); err != nil {
		t.Fatal(err)
	}
	if err := plugin.Validate(attrs, nil); err != nil {
		t.Fatal(err)
	}

	// should resolve the integrated image by digest and allow it
	attrs = admission.NewAttributesRecord(
		&kapi.Pod{Spec: kapi.PodSpec{Containers: []kapi.Container{{Image: "integrated.registry/repo/mysql@" + goodSHA}}}},
		nil, schema.GroupVersionKind{Version: "v1", Kind: "Pod"},
		"default", "pod1", schema.GroupVersionResource{Version: "v1", Resource: "pods"},
		"", admission.Create, false, nil,
	)
	if err := plugin.Admit(attrs, nil); err != nil {
		t.Fatal(err)
	}
	if err := plugin.Validate(attrs, nil); err != nil {
		t.Fatal(err)
	}

	// should attempt resolve the integrated image by tag and fail because tag not found
	attrs = admission.NewAttributesRecord(
		&kapi.Pod{Spec: kapi.PodSpec{Containers: []kapi.Container{{Image: "integrated.registry/repo/mysql:missingtag"}}}},
		nil, schema.GroupVersionKind{Version: "v1", Kind: "Pod"},
		"default", "pod1", schema.GroupVersionResource{Version: "v1", Resource: "pods"},
		"", admission.Create, false, nil,
	)
	if err := plugin.Admit(attrs, nil); err != nil {
		t.Fatal(err)
	}
	if err := plugin.Validate(attrs, nil); err != nil {
		t.Fatal(err)
	}

	// should attempt resolve the integrated image by tag and allow it
	attrs = admission.NewAttributesRecord(
		&kapi.Pod{Spec: kapi.PodSpec{Containers: []kapi.Container{{Image: "integrated.registry/repo/mysql:goodtag"}}}},
		nil, schema.GroupVersionKind{Version: "v1", Kind: "Pod"},
		"default", "pod1", schema.GroupVersionResource{Version: "v1", Resource: "pods"},
		"", admission.Create, false, nil,
	)
	if err := plugin.Admit(attrs, nil); err != nil {
		t.Fatal(err)
	}
	if err := plugin.Validate(attrs, nil); err != nil {
		t.Fatal(err)
	}

	// should attempt resolve the integrated image by tag and forbid it
	attrs = admission.NewAttributesRecord(
		&kapi.Pod{Spec: kapi.PodSpec{Containers: []kapi.Container{{Image: "integrated.registry/repo/mysql:badtag"}}}},
		nil, schema.GroupVersionKind{Version: "v1", Kind: "Pod"},
		"default", "pod1", schema.GroupVersionResource{Version: "v1", Resource: "pods"},
		"", admission.Create, false, nil,
	)
	t.Logf("%#v", plugin.accepter)
	if err := plugin.Admit(attrs, nil); err == nil || !kerrors.IsInvalid(err) {
		t.Fatal(err)
	}
	if err := plugin.Validate(attrs, nil); err == nil || !kerrors.IsInvalid(err) {
		t.Fatal(err)
	}

	// should reject the non-integrated image due to the annotation
	attrs = admission.NewAttributesRecord(
		&kapi.Pod{Spec: kapi.PodSpec{Containers: []kapi.Container{{Image: "index.docker.io/mysql@" + badSHA}}}},
		nil, schema.GroupVersionKind{Version: "v1", Kind: "Pod"},
		"default", "pod1", schema.GroupVersionResource{Version: "v1", Resource: "pods"},
		"", admission.Create, false, nil,
	)
	if err := plugin.Admit(attrs, nil); err == nil || !kerrors.IsInvalid(err) {
		t.Fatal(err)
	}
	if err := plugin.Validate(attrs, nil); err == nil || !kerrors.IsInvalid(err) {
		t.Fatal(err)
	}

	// should reject the non-integrated image due to the annotation on an init container
	attrs = admission.NewAttributesRecord(
		&kapi.Pod{Spec: kapi.PodSpec{InitContainers: []kapi.Container{{Image: "index.docker.io/mysql@" + badSHA}}}},
		nil, schema.GroupVersionKind{Version: "v1", Kind: "Pod"},
		"default", "pod1", schema.GroupVersionResource{Version: "v1", Resource: "pods"},
		"", admission.Create, false, nil,
	)
	if err := plugin.Admit(attrs, nil); err == nil || !kerrors.IsInvalid(err) {
		t.Fatal(err)
	}
	if err := plugin.Validate(attrs, nil); err == nil || !kerrors.IsInvalid(err) {
		t.Fatal(err)
	}

	// should reject the non-integrated image due to the annotation for a build
	attrs = admission.NewAttributesRecord(
		&buildapi.Build{Spec: buildapi.BuildSpec{CommonSpec: buildapi.CommonSpec{Source: buildapi.BuildSource{Images: []buildapi.ImageSource{
			{From: kapi.ObjectReference{Kind: "DockerImage", Name: "index.docker.io/mysql@" + badSHA}},
		}}}}},
		nil, buildGroupVersionKind,
		"default", "build1", buildGroupVersionResource,
		"", admission.Create, false, nil,
	)
	if err := plugin.Admit(attrs, nil); err == nil || !kerrors.IsInvalid(err) {
		t.Fatal(err)
	}
	if err := plugin.Validate(attrs, nil); err == nil || !kerrors.IsInvalid(err) {
		t.Fatal(err)
	}
	attrs = admission.NewAttributesRecord(
		&buildapi.Build{Spec: buildapi.BuildSpec{CommonSpec: buildapi.CommonSpec{Strategy: buildapi.BuildStrategy{DockerStrategy: &buildapi.DockerBuildStrategy{
			From: &kapi.ObjectReference{Kind: "DockerImage", Name: "index.docker.io/mysql@" + badSHA},
		}}}}},
		nil, buildGroupVersionKind,
		"default", "build1", buildGroupVersionResource,
		"", admission.Create, false, nil,
	)
	if err := plugin.Admit(attrs, nil); err == nil || !kerrors.IsInvalid(err) {
		t.Fatal(err)
	}
	if err := plugin.Validate(attrs, nil); err == nil || !kerrors.IsInvalid(err) {
		t.Fatal(err)
	}
	attrs = admission.NewAttributesRecord(
		&buildapi.Build{Spec: buildapi.BuildSpec{CommonSpec: buildapi.CommonSpec{Strategy: buildapi.BuildStrategy{SourceStrategy: &buildapi.SourceBuildStrategy{
			From: kapi.ObjectReference{Kind: "DockerImage", Name: "index.docker.io/mysql@" + badSHA},
		}}}}},
		nil, buildGroupVersionKind,
		"default", "build1", buildGroupVersionResource,
		"", admission.Create, false, nil,
	)
	if err := plugin.Admit(attrs, nil); err == nil || !kerrors.IsInvalid(err) {
		t.Fatal(err)
	}
	if err := plugin.Validate(attrs, nil); err == nil || !kerrors.IsInvalid(err) {
		t.Fatal(err)
	}
	attrs = admission.NewAttributesRecord(
		&buildapi.Build{Spec: buildapi.BuildSpec{CommonSpec: buildapi.CommonSpec{Strategy: buildapi.BuildStrategy{CustomStrategy: &buildapi.CustomBuildStrategy{
			From: kapi.ObjectReference{Kind: "DockerImage", Name: "index.docker.io/mysql@" + badSHA},
		}}}}},
		nil, buildGroupVersionKind,
		"default", "build1", buildGroupVersionResource,
		"", admission.Create, false, nil,
	)
	if err := plugin.Admit(attrs, nil); err == nil || !kerrors.IsInvalid(err) {
		t.Fatal(err)
	}
	if err := plugin.Validate(attrs, nil); err == nil || !kerrors.IsInvalid(err) {
		t.Fatal(err)
	}

	// should allow the non-integrated image due to the annotation for a build config because it's not in the list, even though it has
	// a valid spec
	attrs = admission.NewAttributesRecord(
		&buildapi.BuildConfig{Spec: buildapi.BuildConfigSpec{CommonSpec: buildapi.CommonSpec{Source: buildapi.BuildSource{Images: []buildapi.ImageSource{
			{From: kapi.ObjectReference{Kind: "DockerImage", Name: "index.docker.io/mysql@" + badSHA}},
		}}}}},
		nil, buildConfigGroupVersionKind,
		"default", "build1", buildConfigGroupVersionResource,
		"", admission.Create, false, nil,
	)
	if err := plugin.Admit(attrs, nil); err != nil {
		t.Fatal(err)
	}
	if err := plugin.Validate(attrs, nil); err != nil {
		t.Fatal(err)
	}

	// should hit the cache on the previously good image and continue to allow it (the copy in cache was previously safe)
	goodImage.Annotations = map[string]string{"images.openshift.io/deny-execution": "true"}
	attrs = admission.NewAttributesRecord(
		&kapi.Pod{Spec: kapi.PodSpec{Containers: []kapi.Container{{Image: "index.docker.io/mysql@" + goodSHA}}}},
		nil, schema.GroupVersionKind{Version: "v1", Kind: "Pod"},
		"default", "pod1", schema.GroupVersionResource{Version: "v1", Resource: "pods"},
		"", admission.Create, false, nil,
	)
	if err := plugin.Admit(attrs, nil); err != nil {
		t.Fatal(err)
	}
	if err := plugin.Validate(attrs, nil); err != nil {
		t.Fatal(err)
	}

	// moving 2 minutes in the future should bypass the cache and deny the image
	now = func() time.Time { return time.Unix(1, 0).Add(2 * time.Minute) }
	attrs = admission.NewAttributesRecord(
		&kapi.Pod{Spec: kapi.PodSpec{Containers: []kapi.Container{{Image: "index.docker.io/mysql@" + goodSHA}}}},
		nil, schema.GroupVersionKind{Version: "v1", Kind: "Pod"},
		"default", "pod1", schema.GroupVersionResource{Version: "v1", Resource: "pods"},
		"", admission.Create, false, nil,
	)
	if err := plugin.Admit(attrs, nil); err == nil || !kerrors.IsInvalid(err) {
		t.Fatal(err)
	}
	if err := plugin.Validate(attrs, nil); err == nil || !kerrors.IsInvalid(err) {
		t.Fatal(err)
	}

	// setting a namespace annotation should allow the rule to be skipped immediately
	store.Add(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "",
			Name:      "default",
			Annotations: map[string]string{
				imagepolicy.IgnorePolicyRulesAnnotation: "execution-denied",
			},
		},
	})
	attrs = admission.NewAttributesRecord(
		&kapi.Pod{Spec: kapi.PodSpec{Containers: []kapi.Container{{Image: "index.docker.io/mysql@" + goodSHA}}}},
		nil, schema.GroupVersionKind{Version: "v1", Kind: "Pod"},
		"default", "pod1", schema.GroupVersionResource{Version: "v1", Resource: "pods"},
		"", admission.Create, false, nil,
	)
	if err := plugin.Admit(attrs, nil); err != nil {
		t.Fatal(err)
	}
	if err := plugin.Validate(attrs, nil); err != nil {
		t.Fatal(err)
	}
}

func TestAdmissionWithoutPodSpec(t *testing.T) {
	onResources := []metav1.GroupResource{{Resource: "nodes"}}
	p, err := newImagePolicyPlugin(&imagepolicy.ImagePolicyConfig{
		ExecutionRules: []imagepolicy.ImageExecutionPolicyRule{
			{ImageCondition: imagepolicy.ImageCondition{OnResources: onResources}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	attrs := admission.NewAttributesRecord(
		&kapi.Node{},
		nil, schema.GroupVersionKind{Version: "v1", Kind: "Node"},
		"", "node1", schema.GroupVersionResource{Version: "v1", Resource: "nodes"},
		"", admission.Create, false, nil,
	)
	if err := p.Admit(attrs, nil); !kerrors.IsForbidden(err) || !strings.Contains(err.Error(), "No list of images available for this object") {
		t.Fatal(err)
	}
	if err := p.Validate(attrs, nil); !kerrors.IsForbidden(err) || !strings.Contains(err.Error(), "No list of images available for this object") {
		t.Fatal(err)
	}
}

func TestAdmissionResolution(t *testing.T) {
	onResources := []metav1.GroupResource{{Resource: "pods"}}
	p, err := newImagePolicyPlugin(&imagepolicy.ImagePolicyConfig{
		ResolveImages: imagepolicy.AttemptRewrite,
		ExecutionRules: []imagepolicy.ImageExecutionPolicyRule{
			{ImageCondition: imagepolicy.ImageCondition{OnResources: onResources}},
			{Reject: true, ImageCondition: imagepolicy.ImageCondition{
				OnResources:     onResources,
				MatchRegistries: []string{"index.docker.io"},
			}},
		},
	})
	setDefaultCache(p)

	p.resolver = resolveFunc(func(ref *kapi.ObjectReference, defaultNamespace string, forceLocalResolve bool) (*rules.ImagePolicyAttributes,
		error) {
		switch ref.Name {
		case "index.docker.io/mysql:latest":
			return &rules.ImagePolicyAttributes{
				Name:  reference.DockerImageReference{Registry: "index.docker.io", Name: "mysql", Tag: "latest"},
				Image: &imagev1.Image{ObjectMeta: metav1.ObjectMeta{Name: "1"}},
			}, nil
		case "myregistry.com/mysql/mysql:latest",
			"myregistry.com/mysql/mysql@sha256:a3ed95caeb02ffe68cdd9fd84406680ae93d633cb16422d00e8a7c22955b46d4":
			return &rules.ImagePolicyAttributes{
				Name:  reference.DockerImageReference{Registry: "myregistry.com", Namespace: "mysql", Name: "mysql", ID: "sha256:a3ed95caeb02ffe68cdd9fd84406680ae93d633cb16422d00e8a7c22955b46d4"},
				Image: &imagev1.Image{ObjectMeta: metav1.ObjectMeta{Name: "2"}},
			}, nil
		}
		t.Fatalf("unexpected call to resolve image: %v", ref)
		return nil, nil
	})

	if err != nil {
		t.Fatal(err)
	}
	if !p.Handles(admission.Create) {
		t.Fatal("expected to handle create")
	}
	failingAttrs := admission.NewAttributesRecord(
		&kapi.Pod{
			Spec: kapi.PodSpec{
				Containers: []kapi.Container{
					{Image: "index.docker.io/mysql:latest"},
				},
			},
		},
		nil, schema.GroupVersionKind{Version: "v1", Kind: "Pod"},
		"default", "pod1", schema.GroupVersionResource{Version: "v1", Resource: "pods"},
		"", admission.Create,
		false,
		nil,
	)
	if err := p.Admit(failingAttrs, nil); err == nil {
		t.Fatal(err)
	}
	if err := p.Validate(failingAttrs, nil); err == nil {
		t.Fatal(err)
	}

	pod := &kapi.Pod{
		Spec: kapi.PodSpec{
			Containers: []kapi.Container{
				{Image: "myregistry.com/mysql/mysql:latest"},
				{Image: "myregistry.com/mysql/mysql:latest"},
			},
		},
	}
	attrs := admission.NewAttributesRecord(
		pod,
		nil, schema.GroupVersionKind{Version: "v1", Kind: "Pod"},
		"default", "pod1", schema.GroupVersionResource{Version: "v1", Resource: "pods"},
		"", admission.Create, false, nil,
	)
	if err := p.Admit(attrs, nil); err != nil {
		t.Logf("object: %#v", attrs.GetObject())
		t.Fatal(err)
	}
	if pod.Spec.Containers[0].Image != "myregistry.com/mysql/mysql@sha256:a3ed95caeb02ffe68cdd9fd84406680ae93d633cb16422d00e8a7c22955b46d4" ||
		pod.Spec.Containers[1].Image != "myregistry.com/mysql/mysql@sha256:a3ed95caeb02ffe68cdd9fd84406680ae93d633cb16422d00e8a7c22955b46d4" {
		t.Errorf("unexpected image: %#v", pod)
	}
	if err := p.Validate(attrs, nil); err != nil {
		t.Logf("object: %#v", attrs.GetObject())
		t.Fatal(err)
	}
	if pod.Spec.Containers[0].Image != "myregistry.com/mysql/mysql@sha256:a3ed95caeb02ffe68cdd9fd84406680ae93d633cb16422d00e8a7c22955b46d4" ||
		pod.Spec.Containers[1].Image != "myregistry.com/mysql/mysql@sha256:a3ed95caeb02ffe68cdd9fd84406680ae93d633cb16422d00e8a7c22955b46d4" {
		t.Errorf("unexpected image: %#v", pod)
	}

	// Simulate a later admission plugin modifying the pod spec back to something that requires resolution
	pod.Spec.Containers[0].Image = "myregistry.com/mysql/mysql:latest"
	if err := p.Validate(attrs, nil); err == nil {
		t.Fatal("expected validate error on mutation, got none")
	} else if !strings.Contains(err.Error(), "changed after admission") {
		t.Fatalf("expected mutation-related error, got %v", err)
	}
}

func TestAdmissionResolveImages(t *testing.T) {
	image1 := &imagev1.Image{
		ObjectMeta:           metav1.ObjectMeta{Name: "sha256:0000000000000000000000000000000000000000000000000000000000000001"},
		DockerImageReference: "integrated.registry/image1/image1@sha256:0000000000000000000000000000000000000000000000000000000000000001",
	}

	defaultPolicyConfig := &imagepolicy.ImagePolicyConfig{}
	configContent, err := ioutil.ReadAll(bytes.NewBufferString(`{"kind":"ImagePolicyConfig","apiVersion":"image.openshift.io/v1"}`))
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	utilruntime.Must(imagepolicy.Install(scheme))
	codecs := serializer.NewCodecFactory(scheme)
	err = runtime.DecodeInto(codecs.UniversalDecoder(imagepolicy.GroupVersion), configContent, defaultPolicyConfig)
	if err != nil {
		t.Fatal(err)
	}
	imagepolicy.SetDefaults_ImagePolicyConfig(defaultPolicyConfig)

	testCases := []struct {
		name   string
		client *imagev1fakeclient.Clientset
		policy imagepolicy.ImageResolutionType
		config *imagepolicy.ImagePolicyConfig
		attrs  admission.Attributes
		admit  bool
		expect runtime.Object
	}{

		{
			name:   "fails resolution",
			policy: imagepolicy.RequiredRewrite,
			client: imagev1fakeclient.NewSimpleClientset(),
			attrs: admission.NewAttributesRecord(
				&kapi.Pod{
					Spec: kapi.PodSpec{
						Containers: []kapi.Container{
							{Image: "integrated.registry/test/mysql@" + goodSHA},
						},
						InitContainers: []kapi.Container{
							{Image: "myregistry.com/mysql/mysql:latest"},
						},
					},
				}, nil, schema.GroupVersionKind{Version: "v1", Kind: "Pod"},
				"default", "pod1", schema.GroupVersionResource{Version: "v1", Resource: "pods"},
				"", admission.Create, false, nil,
			),
		},
		{
			name:   "resolves images in the integrated registry without altering their ref (avoids looking up the tag)",
			policy: imagepolicy.RequiredRewrite,
			client: imagev1fakeclient.NewSimpleClientset(
				image1,
			),
			attrs: admission.NewAttributesRecord(
				&kapi.Pod{
					Spec: kapi.PodSpec{
						Containers: []kapi.Container{
							{Image: "integrated.registry/test/mysql@sha256:0000000000000000000000000000000000000000000000000000000000000001"},
						},
					},
				}, nil, schema.GroupVersionKind{Version: "v1", Kind: "Pod"},
				"default", "pod1", schema.GroupVersionResource{Version: "v1", Resource: "pods"},
				"", admission.Create, false, nil,
			),
			admit: true,
			expect: &kapi.Pod{
				Spec: kapi.PodSpec{
					Containers: []kapi.Container{
						{Image: "integrated.registry/test/mysql@sha256:0000000000000000000000000000000000000000000000000000000000000001"},
					},
				},
			},
		},
		{
			name:   "resolves images in the integrated registry without altering their ref (avoids looking up the tag)",
			policy: imagepolicy.RequiredRewrite,
			client: imagev1fakeclient.NewSimpleClientset(
				image1,
			),
			attrs: admission.NewAttributesRecord(
				&kapi.Pod{
					Spec: kapi.PodSpec{
						InitContainers: []kapi.Container{
							{Image: "integrated.registry/test/mysql@sha256:0000000000000000000000000000000000000000000000000000000000000001"},
						},
					},
				}, nil, schema.GroupVersionKind{Version: "v1", Kind: "Pod"},
				"default", "pod1", schema.GroupVersionResource{Version: "v1", Resource: "pods"},
				"", admission.Create, false, nil,
			),
			admit: true,
			expect: &kapi.Pod{
				Spec: kapi.PodSpec{
					InitContainers: []kapi.Container{
						{Image: "integrated.registry/test/mysql@sha256:0000000000000000000000000000000000000000000000000000000000000001"},
					},
				},
			},
		},
		{
			name:   "resolves images in the integrated registry on builds without altering their ref (avoids looking up the tag)",
			policy: imagepolicy.RequiredRewrite,
			client: imagev1fakeclient.NewSimpleClientset(
				image1,
			),
			attrs: admission.NewAttributesRecord(
				&buildapi.Build{
					Spec: buildapi.BuildSpec{
						CommonSpec: buildapi.CommonSpec{
							Strategy: buildapi.BuildStrategy{
								SourceStrategy: &buildapi.SourceBuildStrategy{
									From: kapi.ObjectReference{Kind: "DockerImage", Name: "integrated.registry/test/mysql@sha256:0000000000000000000000000000000000000000000000000000000000000001"},
								},
							},
						},
					},
				}, nil, buildGroupVersionKind,
				"default", "build1", buildGroupVersionResource,
				"", admission.Create, false, nil,
			),
			admit: true,
			expect: &buildapi.Build{
				Spec: buildapi.BuildSpec{
					CommonSpec: buildapi.CommonSpec{
						Strategy: buildapi.BuildStrategy{
							SourceStrategy: &buildapi.SourceBuildStrategy{
								From: kapi.ObjectReference{Kind: "DockerImage", Name: "integrated.registry/test/mysql@sha256:0000000000000000000000000000000000000000000000000000000000000001"},
							},
						},
					},
				},
			},
		},
		{
			name:   "resolves builds with image stream tags, uses the image DockerImageReference with SHA set",
			policy: imagepolicy.RequiredRewrite,
			client: imagev1fakeclient.NewSimpleClientset(
				&imagev1.ImageStreamTag{
					ObjectMeta: metav1.ObjectMeta{Name: "test:other", Namespace: "default"},
					Image:      *image1,
				},
			),
			attrs: admission.NewAttributesRecord(
				&buildapi.Build{
					Spec: buildapi.BuildSpec{
						CommonSpec: buildapi.CommonSpec{
							Strategy: buildapi.BuildStrategy{
								CustomStrategy: &buildapi.CustomBuildStrategy{
									From: kapi.ObjectReference{Kind: "ImageStreamTag", Name: "test:other"},
								},
							},
						},
					},
				}, nil, buildGroupVersionKind,
				"default", "build1", buildGroupVersionResource,
				"", admission.Create, false, nil,
			),
			admit: true,
			expect: &buildapi.Build{
				Spec: buildapi.BuildSpec{
					CommonSpec: buildapi.CommonSpec{
						Strategy: buildapi.BuildStrategy{
							CustomStrategy: &buildapi.CustomBuildStrategy{
								From: kapi.ObjectReference{Kind: "DockerImage", Name: "integrated.registry/image1/image1@sha256:0000000000000000000000000000000000000000000000000000000000000001"},
							},
						},
					},
				},
			},
		},
		{
			name:   "does not resolve a build update because the reference didn't change",
			policy: imagepolicy.RequiredRewrite,
			client: imagev1fakeclient.NewSimpleClientset(
				&imagev1.ImageStreamTag{
					ObjectMeta: metav1.ObjectMeta{Name: "test:other", Namespace: "default"},
					Image:      *image1,
				},
			),
			attrs: admission.NewAttributesRecord(
				&buildapi.Build{
					Spec: buildapi.BuildSpec{
						CommonSpec: buildapi.CommonSpec{
							Strategy: buildapi.BuildStrategy{
								CustomStrategy: &buildapi.CustomBuildStrategy{
									From: kapi.ObjectReference{Kind: "ImageStreamTag", Name: "test:other"},
								},
							},
						},
					},
				},
				&buildapi.Build{
					Spec: buildapi.BuildSpec{
						CommonSpec: buildapi.CommonSpec{
							Strategy: buildapi.BuildStrategy{
								CustomStrategy: &buildapi.CustomBuildStrategy{
									From: kapi.ObjectReference{Kind: "ImageStreamTag", Name: "test:other"},
								},
							},
						},
					},
				},
				buildGroupVersionKind,
				"default", "build1", buildGroupVersionResource,
				"", admission.Create, false, nil,
			),
			admit: true,
			expect: &buildapi.Build{
				Spec: buildapi.BuildSpec{
					CommonSpec: buildapi.CommonSpec{
						Strategy: buildapi.BuildStrategy{
							CustomStrategy: &buildapi.CustomBuildStrategy{
								From: kapi.ObjectReference{Kind: "ImageStreamTag", Name: "test:other"},
							},
						},
					},
				},
			},
		},
		{
			name:   "resolves images in the integrated registry on builds without altering their ref (avoids looking up the tag)",
			policy: imagepolicy.RequiredRewrite,
			client: imagev1fakeclient.NewSimpleClientset(
				image1,
			),
			attrs: admission.NewAttributesRecord(
				&buildapi.Build{
					Spec: buildapi.BuildSpec{
						CommonSpec: buildapi.CommonSpec{
							Strategy: buildapi.BuildStrategy{
								SourceStrategy: &buildapi.SourceBuildStrategy{
									From: kapi.ObjectReference{Kind: "DockerImage", Name: "integrated.registry/test/mysql@sha256:0000000000000000000000000000000000000000000000000000000000000001"},
								},
							},
						},
					},
				}, nil, buildGroupVersionKind,
				"default", "build1", buildGroupVersionResource,
				"", admission.Create, false, nil,
			),
			admit: true,
			expect: &buildapi.Build{
				Spec: buildapi.BuildSpec{
					CommonSpec: buildapi.CommonSpec{
						Strategy: buildapi.BuildStrategy{
							SourceStrategy: &buildapi.SourceBuildStrategy{
								From: kapi.ObjectReference{Kind: "DockerImage", Name: "integrated.registry/test/mysql@sha256:0000000000000000000000000000000000000000000000000000000000000001"},
							},
						},
					},
				},
			},
		},
		{
			name: "does not rewrite the config because build has DoNotAttempt by default, which overrides global policy",
			config: &imagepolicy.ImagePolicyConfig{
				ResolveImages: imagepolicy.RequiredRewrite,
				ResolutionRules: []imagepolicy.ImageResolutionPolicyRule{
					{TargetResource: metav1.GroupResource{Group: "build.openshift.io", Resource: "builds"}},
				},
			},
			client: imagev1fakeclient.NewSimpleClientset(
				&imagev1.ImageStreamTag{
					ObjectMeta: metav1.ObjectMeta{Name: "test:other", Namespace: "default"},
					Image:      *image1,
				},
			),
			attrs: admission.NewAttributesRecord(
				&buildapi.Build{
					Spec: buildapi.BuildSpec{
						CommonSpec: buildapi.CommonSpec{
							Strategy: buildapi.BuildStrategy{
								CustomStrategy: &buildapi.CustomBuildStrategy{
									From: kapi.ObjectReference{Kind: "ImageStreamTag", Name: "test:other"},
								},
							},
						},
					},
				}, nil, buildGroupVersionKind,
				"default", "build1", buildGroupVersionResource,
				"", admission.Create, false, nil,
			),
			admit: true,
			expect: &buildapi.Build{
				Spec: buildapi.BuildSpec{
					CommonSpec: buildapi.CommonSpec{
						Strategy: buildapi.BuildStrategy{
							CustomStrategy: &buildapi.CustomBuildStrategy{
								From: kapi.ObjectReference{Kind: "ImageStreamTag", Name: "test:other"},
							},
						},
					},
				},
			},
		},
		{
			name: "does not rewrite the config because the default policy uses attempt by default",
			config: &imagepolicy.ImagePolicyConfig{
				ResolveImages: imagepolicy.RequiredRewrite,
				ResolutionRules: []imagepolicy.ImageResolutionPolicyRule{
					{TargetResource: metav1.GroupResource{Group: "build.openshift.io", Resource: "builds"}, Policy: imagepolicy.Attempt},
				},
			},
			client: imagev1fakeclient.NewSimpleClientset(
				&imagev1.ImageStreamTag{
					ObjectMeta: metav1.ObjectMeta{Name: "test:other", Namespace: "default"},
					Image:      *image1,
				},
			),
			attrs: admission.NewAttributesRecord(
				&buildapi.Build{
					Spec: buildapi.BuildSpec{
						CommonSpec: buildapi.CommonSpec{
							Strategy: buildapi.BuildStrategy{
								CustomStrategy: &buildapi.CustomBuildStrategy{
									From: kapi.ObjectReference{Kind: "ImageStreamTag", Name: "test:other"},
								},
							},
						},
					},
				}, nil, buildGroupVersionKind,
				"default", "build1", buildGroupVersionResource,
				"", admission.Create, false, nil,
			),
			admit: true,
			expect: &buildapi.Build{
				Spec: buildapi.BuildSpec{
					CommonSpec: buildapi.CommonSpec{
						Strategy: buildapi.BuildStrategy{
							CustomStrategy: &buildapi.CustomBuildStrategy{
								From: kapi.ObjectReference{Kind: "ImageStreamTag", Name: "test:other"},
							},
						},
					},
				},
			},
		},
		{
			name: "rewrites the config because build has AttemptRewrite which overrides the global policy",
			config: &imagepolicy.ImagePolicyConfig{
				ResolveImages: imagepolicy.DoNotAttempt,
				ResolutionRules: []imagepolicy.ImageResolutionPolicyRule{
					{TargetResource: metav1.GroupResource{Group: "build.openshift.io", Resource: "builds"}, Policy: imagepolicy.AttemptRewrite},
				},
			},
			client: imagev1fakeclient.NewSimpleClientset(
				&imagev1.ImageStreamTag{
					ObjectMeta: metav1.ObjectMeta{Name: "test:other", Namespace: "default"},
					Image:      *image1,
				},
			),
			attrs: admission.NewAttributesRecord(
				&buildapi.Build{
					Spec: buildapi.BuildSpec{
						CommonSpec: buildapi.CommonSpec{
							Strategy: buildapi.BuildStrategy{
								CustomStrategy: &buildapi.CustomBuildStrategy{
									From: kapi.ObjectReference{Kind: "ImageStreamTag", Name: "test:other"},
								},
							},
						},
					},
				}, nil, buildGroupVersionKind,
				"default", "build1", buildGroupVersionResource,
				"", admission.Create, false, nil,
			),
			admit: true,
			expect: &buildapi.Build{
				Spec: buildapi.BuildSpec{
					CommonSpec: buildapi.CommonSpec{
						Strategy: buildapi.BuildStrategy{
							CustomStrategy: &buildapi.CustomBuildStrategy{
								From: kapi.ObjectReference{Kind: "DockerImage", Name: "integrated.registry/image1/image1@sha256:0000000000000000000000000000000000000000000000000000000000000001"},
							},
						},
					},
				},
			},
		},
		{
			name:   "resolves builds.build.openshift.io with image stream tags, uses the image DockerImageReference with SHA set",
			policy: imagepolicy.RequiredRewrite,
			client: imagev1fakeclient.NewSimpleClientset(
				&imagev1.ImageStreamTag{
					ObjectMeta: metav1.ObjectMeta{Name: "test:other", Namespace: "default"},
					Image:      *image1,
				},
			),
			attrs: admission.NewAttributesRecord(
				&buildapi.Build{
					Spec: buildapi.BuildSpec{
						CommonSpec: buildapi.CommonSpec{
							Strategy: buildapi.BuildStrategy{
								CustomStrategy: &buildapi.CustomBuildStrategy{
									From: kapi.ObjectReference{Kind: "ImageStreamTag", Name: "test:other"},
								},
							},
						},
					},
				}, nil, buildGroupVersionKind,
				"default", "build1", buildGroupVersionResource,
				"", admission.Create, false, nil,
			),
			admit: true,
			expect: &buildapi.Build{
				Spec: buildapi.BuildSpec{
					CommonSpec: buildapi.CommonSpec{
						Strategy: buildapi.BuildStrategy{
							CustomStrategy: &buildapi.CustomBuildStrategy{
								From: kapi.ObjectReference{Kind: "DockerImage", Name: "integrated.registry/image1/image1@sha256:0000000000000000000000000000000000000000000000000000000000000001"},
							},
						},
					},
				},
			},
		},
		{
			name:   "resolves builds with image stream images",
			policy: imagepolicy.RequiredRewrite,
			client: imagev1fakeclient.NewSimpleClientset(
				&imagev1.ImageStreamImage{
					ObjectMeta: metav1.ObjectMeta{Name: "test@sha256:0000000000000000000000000000000000000000000000000000000000000001", Namespace: "default"},
					Image:      *image1,
				},
			),
			attrs: admission.NewAttributesRecord(
				&buildapi.Build{
					Spec: buildapi.BuildSpec{
						CommonSpec: buildapi.CommonSpec{
							Strategy: buildapi.BuildStrategy{
								DockerStrategy: &buildapi.DockerBuildStrategy{
									From: &kapi.ObjectReference{Kind: "ImageStreamImage", Name: "test@sha256:0000000000000000000000000000000000000000000000000000000000000001"},
								},
							},
						},
					},
				}, nil, buildGroupVersionKind,
				"default", "build1", buildGroupVersionResource,
				"", admission.Create, false, nil,
			),
			admit: true,
			expect: &buildapi.Build{
				Spec: buildapi.BuildSpec{
					CommonSpec: buildapi.CommonSpec{
						Strategy: buildapi.BuildStrategy{
							DockerStrategy: &buildapi.DockerBuildStrategy{
								From: &kapi.ObjectReference{Kind: "DockerImage", Name: "integrated.registry/image1/image1@sha256:0000000000000000000000000000000000000000000000000000000000000001"},
							},
						},
					},
				},
			},
		},
		{
			name:   "resolves builds that have a local name to their image stream tags, uses the image DockerImageReference with SHA set",
			policy: imagepolicy.RequiredRewrite,
			client: imagev1fakeclient.NewSimpleClientset(
				&imagev1.ImageStreamTag{
					ObjectMeta:   metav1.ObjectMeta{Name: "test:other", Namespace: "default"},
					LookupPolicy: imagev1.ImageLookupPolicy{Local: true},
					Image:        *image1,
				},
			),
			attrs: admission.NewAttributesRecord(
				&buildapi.Build{
					Spec: buildapi.BuildSpec{
						CommonSpec: buildapi.CommonSpec{
							Strategy: buildapi.BuildStrategy{
								CustomStrategy: &buildapi.CustomBuildStrategy{
									From: kapi.ObjectReference{Kind: "DockerImage", Name: "test:other"},
								},
							},
						},
					},
				}, nil, buildGroupVersionKind,
				"default", "build1", buildGroupVersionResource,
				"", admission.Create, false, nil,
			),
			admit: true,
			expect: &buildapi.Build{
				Spec: buildapi.BuildSpec{
					CommonSpec: buildapi.CommonSpec{
						Strategy: buildapi.BuildStrategy{
							CustomStrategy: &buildapi.CustomBuildStrategy{
								From: kapi.ObjectReference{Kind: "DockerImage", Name: "integrated.registry/image1/image1@sha256:0000000000000000000000000000000000000000000000000000000000000001"},
							},
						},
					},
				},
			},
		},
		{
			name:   "resolves pods",
			policy: imagepolicy.RequiredRewrite,
			client: imagev1fakeclient.NewSimpleClientset(
				&imagev1.ImageStreamTag{
					ObjectMeta:   metav1.ObjectMeta{Name: "test:other", Namespace: "default"},
					LookupPolicy: imagev1.ImageLookupPolicy{Local: true},
					Image:        *image1,
				},
			),
			attrs: admission.NewAttributesRecord(
				&kapi.Pod{
					Spec: kapi.PodSpec{
						Containers: []kapi.Container{
							{Image: "test:other"},
						},
					},
				}, nil, schema.GroupVersionKind{Version: "v1", Kind: "Pod", Group: ""},
				"default", "pod1", schema.GroupVersionResource{Version: "v1", Resource: "pods", Group: ""},
				"", admission.Create, false, nil,
			),
			admit: true,
			expect: &kapi.Pod{
				Spec: kapi.PodSpec{
					Containers: []kapi.Container{
						{Image: "integrated.registry/image1/image1@sha256:0000000000000000000000000000000000000000000000000000000000000001"},
					},
				},
			},
		},
		{
			name:   "resolves replica sets that have a local name to their image stream tags, uses the image DockerImageReference with SHA set",
			policy: imagepolicy.RequiredRewrite,
			client: imagev1fakeclient.NewSimpleClientset(
				&imagev1.ImageStreamTag{
					ObjectMeta:   metav1.ObjectMeta{Name: "test:other", Namespace: "default"},
					LookupPolicy: imagev1.ImageLookupPolicy{Local: true},
					Image:        *image1,
				},
			),
			attrs: admission.NewAttributesRecord(
				&apps.ReplicaSet{
					Spec: apps.ReplicaSetSpec{
						Template: kapi.PodTemplateSpec{
							Spec: kapi.PodSpec{
								Containers: []kapi.Container{
									{Image: "test:other"},
								},
							},
						},
					},
				}, nil, schema.GroupVersionKind{Version: "v1", Kind: "ReplicaSet", Group: "extensions"},
				"default", "rs1", schema.GroupVersionResource{Version: "v1", Resource: "replicasets", Group: "extensions"},
				"", admission.Create, false, nil,
			),
			admit: true,
			expect: &apps.ReplicaSet{
				Spec: apps.ReplicaSetSpec{
					Template: kapi.PodTemplateSpec{
						Spec: kapi.PodSpec{
							Containers: []kapi.Container{
								{Image: "integrated.registry/image1/image1@sha256:0000000000000000000000000000000000000000000000000000000000000001"},
							},
						},
					},
				},
			},
		},
		{
			name:   "does not resolve replica sets by default",
			config: defaultPolicyConfig,
			client: imagev1fakeclient.NewSimpleClientset(
				&imagev1.ImageStreamTag{
					ObjectMeta: metav1.ObjectMeta{Name: "test:other", Namespace: "default"},
					Image:      *image1,
				},
			),
			attrs: admission.NewAttributesRecord(
				&apps.ReplicaSet{
					Spec: apps.ReplicaSetSpec{
						Template: kapi.PodTemplateSpec{
							Spec: kapi.PodSpec{
								Containers: []kapi.Container{
									{Image: "integrated.registry/default/test:other"},
								},
							},
						},
					},
				}, nil, schema.GroupVersionKind{Version: "v1", Kind: "ReplicaSet", Group: "extensions"},
				"default", "rs1", schema.GroupVersionResource{Version: "v1", Resource: "replicasets", Group: "extensions"},
				"", admission.Create, false, nil,
			),
			admit: true,
			expect: &apps.ReplicaSet{
				Spec: apps.ReplicaSetSpec{
					Template: kapi.PodTemplateSpec{
						Spec: kapi.PodSpec{
							Containers: []kapi.Container{
								{Image: "integrated.registry/default/test:other"},
							},
						},
					},
				},
			},
		},
		{
			name:   "resolves replica sets that specifically request lookup",
			policy: imagepolicy.RequiredRewrite,
			client: imagev1fakeclient.NewSimpleClientset(
				&imagev1.ImageStreamTag{
					ObjectMeta:   metav1.ObjectMeta{Name: "test:other", Namespace: "default"},
					LookupPolicy: imagev1.ImageLookupPolicy{Local: false},
					Image:        *image1,
				},
			),
			attrs: admission.NewAttributesRecord(
				&apps.ReplicaSet{
					Spec: apps.ReplicaSetSpec{
						Template: kapi.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{imagepolicy.ResolveNamesAnnotation: "*"}},
							Spec: kapi.PodSpec{
								Containers: []kapi.Container{
									{Image: "test:other"},
								},
							},
						},
					},
				}, nil, schema.GroupVersionKind{Version: "v1", Kind: "ReplicaSet", Group: "extensions"},
				"default", "rs1", schema.GroupVersionResource{Version: "v1", Resource: "replicasets", Group: "extensions"},
				"", admission.Create, false, nil,
			),
			admit: true,
			expect: &apps.ReplicaSet{
				Spec: apps.ReplicaSetSpec{
					Template: kapi.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{imagepolicy.ResolveNamesAnnotation: "*"}},
						Spec: kapi.PodSpec{
							Containers: []kapi.Container{
								{Image: "integrated.registry/image1/image1@sha256:0000000000000000000000000000000000000000000000000000000000000001"},
							},
						},
					},
				},
			},
		},
		{
			name:   "if the tag is not found, but the stream is and resolves, resolve to the tag",
			policy: imagepolicy.AttemptRewrite,
			client: (func() *imagev1fakeclient.Clientset {
				fake := &imagev1fakeclient.Clientset{}
				fake.AddReactor("get", "imagestreamtags", func(action clientgotesting.Action) (handled bool, ret runtime.Object, err error) {
					return true, nil, kerrors.NewNotFound(schema.GroupResource{Group: "image.openshift.io", Resource: "imagestreamtags"}, "test:other")
				})
				fake.AddReactor("get", "imagestreams", func(action clientgotesting.Action) (handled bool, ret runtime.Object, err error) {
					return true, &imagev1.ImageStream{
						ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
						Spec: imagev1.ImageStreamSpec{
							LookupPolicy: imagev1.ImageLookupPolicy{Local: true},
						},
						Status: imagev1.ImageStreamStatus{
							DockerImageRepository: "integrated.registry:5000/default/test",
						},
					}, nil
				})
				return fake
			})(),
			attrs: admission.NewAttributesRecord(
				&apps.ReplicaSet{
					Spec: apps.ReplicaSetSpec{
						Template: kapi.PodTemplateSpec{
							Spec: kapi.PodSpec{
								Containers: []kapi.Container{
									{Image: "test:other"},
								},
							},
						},
					},
				}, nil, schema.GroupVersionKind{Version: "v1", Kind: "ReplicaSet", Group: "extensions"},
				"default", "rs1", schema.GroupVersionResource{Version: "v1", Resource: "replicasets", Group: "extensions"},
				"", admission.Create, false, nil,
			),
			admit: true,
			expect: &apps.ReplicaSet{
				Spec: apps.ReplicaSetSpec{
					Template: kapi.PodTemplateSpec{
						Spec: kapi.PodSpec{
							Containers: []kapi.Container{
								{Image: "integrated.registry:5000/default/test:other"},
							},
						},
					},
				},
			},
		},
		{
			name:   "if the tag is not found, but the stream is and doesn't resolve, use the original value",
			policy: imagepolicy.AttemptRewrite,
			client: (func() *imagev1fakeclient.Clientset {
				fake := &imagev1fakeclient.Clientset{}
				fake.AddReactor("get", "imagestreamtags", func(action clientgotesting.Action) (handled bool, ret runtime.Object, err error) {
					return true, nil, kerrors.NewNotFound(schema.GroupResource{Resource: "imagestreamtags"}, "test:other")
				})
				fake.AddReactor("get", "imagestreams", func(action clientgotesting.Action) (handled bool, ret runtime.Object, err error) {
					return true, &imagev1.ImageStream{
						ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
						Spec: imagev1.ImageStreamSpec{
							LookupPolicy: imagev1.ImageLookupPolicy{Local: false},
						},
						Status: imagev1.ImageStreamStatus{
							DockerImageRepository: "integrated.registry:5000/default/test",
						},
					}, nil
				})
				return fake
			})(),
			attrs: admission.NewAttributesRecord(
				&apps.ReplicaSet{
					Spec: apps.ReplicaSetSpec{
						Template: kapi.PodTemplateSpec{
							Spec: kapi.PodSpec{
								Containers: []kapi.Container{
									{Image: "test:other"},
								},
							},
						},
					},
				}, nil, schema.GroupVersionKind{Version: "v1", Kind: "ReplicaSet", Group: "extensions"},
				"default", "rs1", schema.GroupVersionResource{Version: "v1", Resource: "replicasets", Group: "extensions"},
				"", admission.Create, false, nil,
			),
			admit: true,
			expect: &apps.ReplicaSet{
				Spec: apps.ReplicaSetSpec{
					Template: kapi.PodTemplateSpec{
						Spec: kapi.PodSpec{
							Containers: []kapi.Container{
								{Image: "test:other"},
							},
						},
					},
				},
			},
		},
		{
			name:   "if the tag is not found, the stream resolves, but the registry is not installed, don't match",
			policy: imagepolicy.AttemptRewrite,
			client: (func() *imagev1fakeclient.Clientset {
				fake := &imagev1fakeclient.Clientset{}
				fake.AddReactor("get", "imagestreamtags", func(action clientgotesting.Action) (handled bool, ret runtime.Object, err error) {
					return true, nil, kerrors.NewNotFound(schema.GroupResource{Resource: "imagestreamtags"}, "test:other")
				})
				fake.AddReactor("get", "imagestreams", func(action clientgotesting.Action) (handled bool, ret runtime.Object, err error) {
					return true, &imagev1.ImageStream{
						ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
						Spec: imagev1.ImageStreamSpec{
							LookupPolicy: imagev1.ImageLookupPolicy{Local: true},
						},
						Status: imagev1.ImageStreamStatus{
							DockerImageRepository: "",
						},
					}, nil
				})
				return fake
			})(),
			attrs: admission.NewAttributesRecord(
				&apps.ReplicaSet{
					Spec: apps.ReplicaSetSpec{
						Template: kapi.PodTemplateSpec{
							Spec: kapi.PodSpec{
								Containers: []kapi.Container{
									{Image: "test:other"},
								},
							},
						},
					},
				}, nil, schema.GroupVersionKind{Version: "v1", Kind: "ReplicaSet", Group: "extensions"},
				"default", "rs1", schema.GroupVersionResource{Version: "v1", Resource: "replicasets", Group: "extensions"},
				"", admission.Create, false, nil,
			),
			admit: true,
			expect: &apps.ReplicaSet{
				Spec: apps.ReplicaSetSpec{
					Template: kapi.PodTemplateSpec{
						Spec: kapi.PodSpec{
							Containers: []kapi.Container{
								{Image: "test:other"},
							},
						},
					},
				},
			},
		},
	}
	for i, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			onResources := []metav1.GroupResource{{Group: "build.openshift.io", Resource: "builds"}, {Resource: "pods"}}
			config := test.config
			if config == nil {
				// old style config
				config = &imagepolicy.ImagePolicyConfig{
					ResolveImages: test.policy,
					ResolutionRules: []imagepolicy.ImageResolutionPolicyRule{
						{LocalNames: true, TargetResource: metav1.GroupResource{Resource: "*"}, Policy: test.policy},
						{LocalNames: true, TargetResource: metav1.GroupResource{Group: "extensions", Resource: "*"}, Policy: test.policy},
					},
					ExecutionRules: []imagepolicy.ImageExecutionPolicyRule{
						{ImageCondition: imagepolicy.ImageCondition{OnResources: onResources}},
					},
				}
			}
			p, err := newImagePolicyPlugin(config)
			if err != nil {
				t.Fatal(err)
			}

			setDefaultCache(p)
			p.client = test.client
			p.SetDefaultRegistryFunc(func() (string, bool) {
				return "integrated.registry", true
			})
			if err := p.ValidateInitialization(); err != nil {
				t.Fatal(err)
			}

			if err := p.Admit(test.attrs, nil); err != nil {
				if test.admit {
					t.Errorf("%d: should admit: %v", i, err)
				}
				return
			}
			if !test.admit {
				t.Errorf("%d: should not admit", i)
				return
			}
			if !reflect.DeepEqual(test.expect, test.attrs.GetObject()) {
				t.Errorf("%d: unequal: %s", i, diff.ObjectReflectDiff(test.expect, test.attrs.GetObject()))
			}

			if err := p.Validate(test.attrs, nil); err != nil {
				t.Errorf("%d: should validate: %v", i, err)
				return
			}
			if !reflect.DeepEqual(test.expect, test.attrs.GetObject()) {
				t.Errorf("%d: unequal: %s", i, diff.ObjectReflectDiff(test.expect, test.attrs.GetObject()))
			}
		})
	}
}

func TestResolutionConfig(t *testing.T) {
	testCases := []struct {
		config   *imagepolicy.ImagePolicyConfig
		resource metav1.GroupResource
		attrs    rules.ImagePolicyAttributes
		update   bool

		resolve bool
		fail    bool
		rewrite bool
	}{
		{
			config:  &imagepolicy.ImagePolicyConfig{ResolveImages: imagepolicy.AttemptRewrite},
			resolve: true,
			rewrite: true,
		},
		// requires local rewrite for local names
		{
			config: &imagepolicy.ImagePolicyConfig{
				ResolveImages: imagepolicy.DoNotAttempt,
				ResolutionRules: []imagepolicy.ImageResolutionPolicyRule{
					{LocalNames: true, TargetResource: metav1.GroupResource{Resource: "*"}},
				},
			},
			resolve: true,
			rewrite: false,
		},
		// wildcard resource matches
		{
			attrs: rules.ImagePolicyAttributes{LocalRewrite: true},
			config: &imagepolicy.ImagePolicyConfig{
				ResolveImages: imagepolicy.DoNotAttempt,
				ResolutionRules: []imagepolicy.ImageResolutionPolicyRule{
					{LocalNames: true, TargetResource: metav1.GroupResource{Resource: "*"}},
				},
			},
			resolve: true,
			rewrite: true,
		},
		// group mismatch fails
		{
			attrs: rules.ImagePolicyAttributes{LocalRewrite: true},
			config: &imagepolicy.ImagePolicyConfig{
				ResolveImages: imagepolicy.DoNotAttempt,
				ResolutionRules: []imagepolicy.ImageResolutionPolicyRule{
					{LocalNames: true, TargetResource: metav1.GroupResource{Group: "test", Resource: "*"}},
				},
			},
			resource: metav1.GroupResource{Group: "other"},
			resolve:  false,
			rewrite:  false,
		},
		// resource mismatch fails
		{
			attrs: rules.ImagePolicyAttributes{LocalRewrite: true},
			config: &imagepolicy.ImagePolicyConfig{
				ResolveImages: imagepolicy.DoNotAttempt,
				ResolutionRules: []imagepolicy.ImageResolutionPolicyRule{
					{LocalNames: true, TargetResource: metav1.GroupResource{Group: "test", Resource: "self"}},
				},
			},
			resource: metav1.GroupResource{Group: "test", Resource: "other"},
			resolve:  false,
			rewrite:  false,
		},
		// resource match succeeds
		{
			attrs: rules.ImagePolicyAttributes{LocalRewrite: true},
			config: &imagepolicy.ImagePolicyConfig{
				ResolveImages: imagepolicy.DoNotAttempt,
				ResolutionRules: []imagepolicy.ImageResolutionPolicyRule{
					{LocalNames: true, TargetResource: metav1.GroupResource{Group: "test", Resource: "self"}},
				},
			},
			resource: metav1.GroupResource{Group: "test", Resource: "self"},
			resolve:  true,
			rewrite:  true,
		},
		// resource match skips on job update
		{
			attrs: rules.ImagePolicyAttributes{LocalRewrite: true},
			config: &imagepolicy.ImagePolicyConfig{
				ResolveImages: imagepolicy.DoNotAttempt,
				ResolutionRules: []imagepolicy.ImageResolutionPolicyRule{
					{LocalNames: true, TargetResource: metav1.GroupResource{Group: "batch", Resource: "jobs"}},
				},
			},
			resource: metav1.GroupResource{Group: "batch", Resource: "jobs"},
			update:   true,
			resolve:  true,
			rewrite:  false,
		},
		// resource match succeeds on job create
		{
			attrs: rules.ImagePolicyAttributes{LocalRewrite: true},
			config: &imagepolicy.ImagePolicyConfig{
				ResolveImages: imagepolicy.DoNotAttempt,
				ResolutionRules: []imagepolicy.ImageResolutionPolicyRule{
					{LocalNames: true, TargetResource: metav1.GroupResource{Group: "batch", Resource: "jobs"}},
				},
			},
			resource: metav1.GroupResource{Group: "batch", Resource: "jobs"},
			update:   false,
			resolve:  true,
			rewrite:  true,
		},
		// resource match skips on build update
		{
			attrs: rules.ImagePolicyAttributes{LocalRewrite: true},
			config: &imagepolicy.ImagePolicyConfig{
				ResolveImages: imagepolicy.DoNotAttempt,
				ResolutionRules: []imagepolicy.ImageResolutionPolicyRule{
					{LocalNames: true, TargetResource: metav1.GroupResource{Group: "build.openshift.io", Resource: "builds"}},
				},
			},
			resource: metav1.GroupResource{Group: "build.openshift.io", Resource: "builds"},
			update:   true,
			resolve:  true,
			rewrite:  false,
		},
		// resource match skips on statefulset update
		// TODO: remove in 3.7
		{
			attrs: rules.ImagePolicyAttributes{LocalRewrite: true},
			config: &imagepolicy.ImagePolicyConfig{
				ResolveImages: imagepolicy.DoNotAttempt,
				ResolutionRules: []imagepolicy.ImageResolutionPolicyRule{
					{LocalNames: true, TargetResource: metav1.GroupResource{Group: "apps", Resource: "statefulsets"}},
				},
			},
			resource: metav1.GroupResource{Group: "apps", Resource: "statefulsets"},
			update:   true,
			resolve:  true,
			rewrite:  false,
		},
	}

	for i, test := range testCases {
		c := resolutionConfig{test.config}
		if c.RequestsResolution(test.resource) != test.resolve {
			t.Errorf("%d: request resolution != %t", i, test.resolve)
		}
		if c.FailOnResolutionFailure(test.resource) != test.fail {
			t.Errorf("%d: resolution failure != %t", i, test.fail)
		}
		if c.RewriteImagePullSpec(&test.attrs, test.update, test.resource) != test.rewrite {
			t.Errorf("%d: rewrite != %t", i, test.rewrite)
		}
	}
}
