package imageapis

import (
	"fmt"
	"os"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kapi "k8s.io/kubernetes/pkg/apis/core"

	g "github.com/onsi/ginkgo"
	o "github.com/onsi/gomega"

	imagev1 "github.com/openshift/api/image/v1"
	"github.com/openshift/library-go/pkg/image/reference"
	quotautil "github.com/openshift/openshift-apiserver/pkg/quota/quotautil"
	imageapi "github.com/openshift/origin/pkg/image/apis/image"
	imagesutil "github.com/openshift/origin/test/extended/images"
	exutil "github.com/openshift/origin/test/extended/util"
)

const (
	limitRangeName = "limits"
	imageSize      = 100
)

var _ = g.Describe("[Feature:ImageQuota][registry][Serial][Suite:openshift/registry/serial] Image limit range", func() {
	defer g.GinkgoRecover()

	var oc = exutil.NewCLI("limitrange-admission", exutil.KubeConfigPath())

	g.BeforeEach(func() {
		_, err := exutil.WaitForInternalRegistryHostname(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
	})

	g.It(fmt.Sprintf("should deny a push of built image exceeding %s limit", imageapi.LimitTypeImage), func() {
		_, err := createLimitRangeOfType(oc, imageapi.LimitTypeImage, kapi.ResourceList{
			kapi.ResourceStorage: resource.MustParse("10Ki"),
		})
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By(fmt.Sprintf("trying to push an image exceeding size limit with just 1 layer"))
		err = imagesutil.BuildAndPushImageOfSizeWithBuilder(oc, nil, oc.Namespace(), "sized", "middle", 16000, 1, false)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By(fmt.Sprintf("trying to push an image exceeding size limit in total"))
		err = imagesutil.BuildAndPushImageOfSizeWithBuilder(oc, nil, oc.Namespace(), "sized", "middle", 16000, 5, false)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By(fmt.Sprintf("trying to push an image with one big layer below size limit"))
		err = imagesutil.BuildAndPushImageOfSizeWithBuilder(oc, nil, oc.Namespace(), "sized", "small", 8000, 1, true)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By(fmt.Sprintf("trying to push an image below size limit"))
		err = imagesutil.BuildAndPushImageOfSizeWithBuilder(oc, nil, oc.Namespace(), "sized", "small", 8000, 2, true)
		o.Expect(err).NotTo(o.HaveOccurred())
	})

	g.It(fmt.Sprintf("should deny a push of built image exceeding limit on %s resource", imageapi.ResourceImageStreamImages), func() {
		limits := kapi.ResourceList{
			imageapi.ResourceImageStreamTags:   resource.MustParse("0"),
			imageapi.ResourceImageStreamImages: resource.MustParse("0"),
		}
		_, err := createLimitRangeOfType(oc, imageapi.LimitTypeImageStream, limits)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By(fmt.Sprintf("trying to push image exceeding limits %v", limits))
		err = imagesutil.BuildAndPushImageOfSizeWithBuilder(oc, nil, oc.Namespace(), "sized", "refused", imageSize, 1, false)
		o.Expect(err).NotTo(o.HaveOccurred())

		limits, err = bumpLimit(oc, imageapi.ResourceImageStreamImages, "1")
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By(fmt.Sprintf("trying to push image below limits %v", limits))
		err = imagesutil.BuildAndPushImageOfSizeWithBuilder(oc, nil, oc.Namespace(), "sized", "first", imageSize, 2, true)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By(fmt.Sprintf("trying to push image exceeding limits %v", limits))
		err = imagesutil.BuildAndPushImageOfSizeWithBuilder(oc, nil, oc.Namespace(), "sized", "second", imageSize, 2, false)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By(fmt.Sprintf("trying to push image below limits %v to another image stream", limits))
		err = imagesutil.BuildAndPushImageOfSizeWithBuilder(oc, nil, oc.Namespace(), "another", "second", imageSize, 1, true)
		o.Expect(err).NotTo(o.HaveOccurred())

		limits, err = bumpLimit(oc, imageapi.ResourceImageStreamImages, "2")
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By(fmt.Sprintf("trying to push image below limits %v", limits))
		err = imagesutil.BuildAndPushImageOfSizeWithBuilder(oc, nil, oc.Namespace(), "another", "third", imageSize, 1, true)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By(fmt.Sprintf("trying to push image exceeding limits %v", limits))
		err = imagesutil.BuildAndPushImageOfSizeWithBuilder(oc, nil, oc.Namespace(), "another", "fourth", imageSize, 1, false)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By(`removing tag "second" from "another" image stream`)
		err = oc.ImageClient().ImageV1().ImageStreamTags(oc.Namespace()).Delete("another:second", nil)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By(fmt.Sprintf("trying to push image below limits %v", limits))
		err = imagesutil.BuildAndPushImageOfSizeWithBuilder(oc, nil, oc.Namespace(), "another", "replenish", imageSize, 1, true)
		o.Expect(err).NotTo(o.HaveOccurred())
	})

	g.It(fmt.Sprintf("should deny a docker image reference exceeding limit on %s resource", imageapi.ResourceImageStreamTags), func() {
		tag2Image, err := buildAndPushTestImagesTo(oc, "src", "tag", 2)
		o.Expect(err).NotTo(o.HaveOccurred())

		limit := kapi.ResourceList{imageapi.ResourceImageStreamTags: resource.MustParse("0")}
		_, err = createLimitRangeOfType(oc, imageapi.LimitTypeImageStream, limit)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By(fmt.Sprintf("trying to tag a docker image exceeding limit %v", limit))
		out, err := oc.Run("import-image").Args("stream:dockerimage", "--confirm", "--insecure", "--from", tag2Image["tag1"].DockerImageReference).Output()
		o.Expect(err).To(o.HaveOccurred())
		o.Expect(out).Should(o.ContainSubstring("exceeds the maximum limit"))
		o.Expect(out).Should(o.ContainSubstring(string(imageapi.ResourceImageStreamTags)))

		limit, err = bumpLimit(oc, imageapi.ResourceImageStreamTags, "1")
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By(fmt.Sprintf("trying to tag a docker image below limit %v", limit))
		err = oc.Run("import-image").Args("stream:dockerimage", "--confirm", "--insecure", "--from", tag2Image["tag1"].DockerImageReference).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		err = exutil.WaitForAnImageStreamTag(oc, oc.Namespace(), "stream", "dockerimage")
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By(fmt.Sprintf("trying to tag a docker image exceeding limit %v", limit))
		is, err := oc.ImageClient().ImageV1().ImageStreams(oc.Namespace()).Get("stream", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		upsertSpecTag(&is.Spec.Tags, imagev1.TagReference{
			Name: "foo",
			From: &corev1.ObjectReference{
				Kind: "DockerImage",
				Name: tag2Image["tag2"].DockerImageReference,
			},
			ImportPolicy: imagev1.TagImportPolicy{
				Insecure: true,
			},
		})
		_, err = oc.ImageClient().ImageV1().ImageStreams(oc.Namespace()).Update(is)
		o.Expect(err).To(o.HaveOccurred())
		o.Expect(quotautil.IsErrorQuotaExceeded(err)).Should(o.Equal(true))

		g.By("re-tagging the image under different tag")
		is, err = oc.ImageClient().ImageV1().ImageStreams(oc.Namespace()).Get("stream", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		upsertSpecTag(&is.Spec.Tags, imagev1.TagReference{
			Name: "duplicate",
			From: &corev1.ObjectReference{
				Kind: "DockerImage",
				Name: tag2Image["tag1"].DockerImageReference,
			},
			ImportPolicy: imagev1.TagImportPolicy{
				Insecure: true,
			},
		})
		_, err = oc.ImageClient().ImageV1().ImageStreams(oc.Namespace()).Update(is)
		o.Expect(err).NotTo(o.HaveOccurred())
	})

	g.It(fmt.Sprintf("should deny an import of a repository exceeding limit on %s resource", imageapi.ResourceImageStreamTags), func() {
		maxBulkImport, err := getMaxImagesBulkImportedPerRepository()
		if err != nil {
			g.Skip(err.Error())
			return
		}

		s1tag2Image, err := buildAndPushTestImagesTo(oc, "src1st", "tag", maxBulkImport+1)
		s2tag2Image, err := buildAndPushTestImagesTo(oc, "src2nd", "t", 2)
		o.Expect(err).NotTo(o.HaveOccurred())

		limit := kapi.ResourceList{
			imageapi.ResourceImageStreamTags:   *resource.NewQuantity(int64(maxBulkImport)+1, resource.DecimalSI),
			imageapi.ResourceImageStreamImages: *resource.NewQuantity(int64(maxBulkImport)+1, resource.DecimalSI),
		}
		_, err = createLimitRangeOfType(oc, imageapi.LimitTypeImageStream, limit)
		o.Expect(err).NotTo(o.HaveOccurred())

		s1ref, err := reference.Parse(s1tag2Image["tag1"].DockerImageReference)
		o.Expect(err).NotTo(o.HaveOccurred())
		s1ref.Tag = ""
		s1ref.ID = ""
		s2ref, err := reference.Parse(s2tag2Image["t1"].DockerImageReference)
		o.Expect(err).NotTo(o.HaveOccurred())
		s2ref.Tag = ""
		s2ref.ID = ""

		g.By(fmt.Sprintf("trying to import from repository %q below quota %v", s1ref.Exact(), limit))
		err = oc.Run("import-image").Args("bulkimport", "--confirm", "--insecure", "--all", "--from", s1ref.Exact()).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		err = exutil.WaitForAnImageStreamTag(oc, oc.Namespace(), "bulkimport", "tag1")
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By(fmt.Sprintf("trying to import tags from repository %q exceeding quota %v", s2ref.Exact(), limit))
		out, err := oc.Run("import-image").Args("bulkimport", "--confirm", "--insecure", "--all", "--from", s2ref.Exact()).Output()
		o.Expect(err).To(o.HaveOccurred())
		o.Expect(out).Should(o.ContainSubstring("exceeds the maximum limit"))
		o.Expect(out).Should(o.ContainSubstring(string(imageapi.ResourceImageStreamTags)))
		o.Expect(out).Should(o.ContainSubstring(string(imageapi.ResourceImageStreamImages)))
	})
})

func upsertSpecTag(tags *[]imagev1.TagReference, tagReference imagev1.TagReference) {
	for i := range *tags {
		curr := (*tags)[i]
		if curr.Name == tagReference.Name {
			(*tags)[i] = tagReference
			return
		}
	}
	*tags = append(*tags, tagReference)
}

// buildAndPushTestImagesTo builds a given number of test images. The images are pushed to a new image stream
// of given name under <tagPrefix><X> where X is a number of image starting from 1.
func buildAndPushTestImagesTo(oc *exutil.CLI, isName string, tagPrefix string, numberOfImages int) (tag2Image map[string]imagev1.Image, err error) {
	tag2Image = make(map[string]imagev1.Image)

	for i := 1; i <= numberOfImages; i++ {
		tag := fmt.Sprintf("%s%d", tagPrefix, i)
		err = imagesutil.BuildAndPushImageOfSizeWithBuilder(oc, nil, oc.Namespace(), isName, tag, imageSize, 2, true)
		if err != nil {
			return nil, err
		}
		ist, err := oc.ImageClient().ImageV1().ImageStreamTags(oc.Namespace()).Get(isName+":"+tag, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		tag2Image[tag] = ist.Image
	}

	return
}

// createLimitRangeOfType creates a new limit range object with given limits for given limit type in current namespace
func createLimitRangeOfType(oc *exutil.CLI, limitType kapi.LimitType, maxLimits kapi.ResourceList) (*kapi.LimitRange, error) {
	lr := &kapi.LimitRange{
		ObjectMeta: metav1.ObjectMeta{
			Name: limitRangeName,
		},
		Spec: kapi.LimitRangeSpec{
			Limits: []kapi.LimitRangeItem{
				{
					Type: limitType,
					Max:  maxLimits,
				},
			},
		},
	}

	g.By(fmt.Sprintf("creating limit range object %q with %s limited to: %v", limitRangeName, limitType, maxLimits))
	lr, err := oc.InternalAdminKubeClient().Core().LimitRanges(oc.Namespace()).Create(lr)
	return lr, err
}

// bumpLimit changes the limit value for given resource for all the limit types of limit range object
func bumpLimit(oc *exutil.CLI, resourceName kapi.ResourceName, limit string) (kapi.ResourceList, error) {
	g.By(fmt.Sprintf("bump a limit on resource %q to %s", resourceName, limit))
	lr, err := oc.InternalAdminKubeClient().Core().LimitRanges(oc.Namespace()).Get(limitRangeName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	res := kapi.ResourceList{}

	change := false
	for i := range lr.Spec.Limits {
		item := &lr.Spec.Limits[i]
		if old, exists := item.Max[resourceName]; exists {
			for k, v := range item.Max {
				res[k] = v
			}
			parsed := resource.MustParse(limit)
			if old.Cmp(parsed) != 0 {
				item.Max[resourceName] = parsed
				change = true
			}
		}
	}

	if !change {
		return res, nil
	}
	_, err = oc.InternalAdminKubeClient().Core().LimitRanges(oc.Namespace()).Update(lr)
	return res, err
}

// getMaxImagesBulkImportedPerRepository returns a maximum numbers of images that can be imported from
// repository at once. The value is obtained from environment variable which must be set.
func getMaxImagesBulkImportedPerRepository() (int, error) {
	max := os.Getenv("MAX_IMAGES_BULK_IMPORTED_PER_REPOSITORY")
	if len(max) == 0 {
		return 0, fmt.Errorf("MAX_IMAGES_BULK_IMPORTED_PER_REPOSITORY is not set")
	}
	return strconv.Atoi(max)
}
