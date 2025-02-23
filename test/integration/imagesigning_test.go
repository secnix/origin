package integration

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/diff"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/client-go/kubernetes"
	rbacv1client "k8s.io/client-go/kubernetes/typed/rbac/v1"
	"k8s.io/client-go/rest"

	imagev1 "github.com/openshift/api/image/v1"
	imagev1client "github.com/openshift/client-go/image/clientset/versioned"
	"github.com/openshift/library-go/pkg/config/helpers"
	"github.com/openshift/origin/pkg/oc/cli/admin/policy"
	testutil "github.com/openshift/origin/test/util"
	testserver "github.com/openshift/origin/test/util/server"
)

const testUserName = "bob"

func TestImageAddSignature(t *testing.T) {
	clusterAdminClientConfig, userKubeClient, adminClient, userClient, image, fn := testSetupImageSignatureTest(t, testUserName)
	defer fn()

	if len(image.Signatures) != 0 {
		t.Fatalf("expected empty signatures, not: %s", diff.ObjectDiff(image.Signatures, []imagev1.ImageSignature{}))
	}

	// add some dummy signature
	signature := imagev1.ImageSignature{
		Type:    "unknown",
		Content: []byte("binaryblob"),
	}

	sigName, err := joinImageSignatureName(image.Name, "signaturename")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	signature.Name = sigName

	created, err := userClient.ImageV1().ImageSignatures().Create(&signature)
	if err == nil {
		t.Fatalf("unexpected success updating image signatures")
	}
	if !kerrors.IsForbidden(err) {
		t.Fatalf("expected forbidden error, not: %v", err)
	}

	makeUserAnImageSigner(rbacv1client.NewForConfigOrDie(clusterAdminClientConfig), userKubeClient, testUserName)

	// try to create the signature again
	created, err = userClient.ImageV1().ImageSignatures().Create(&signature)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	image, err = adminClient.ImageV1().Images().Get(image.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(image.Signatures) != 1 {
		t.Fatalf("unexpected number of signatures in created image (%d != %d)", len(image.Signatures), 1)
	}
	for _, sig := range []*imagev1.ImageSignature{created, &image.Signatures[0]} {
		if sig.Name != sigName || sig.Type != "unknown" ||
			!bytes.Equal(sig.Content, []byte("binaryblob")) || len(sig.Conditions) != 0 {
			t.Errorf("unexpected signature received: %#+v", sig)
		}
	}
	compareSignatures(t, image.Signatures[0], *created)

	// try to create the signature yet again
	created, err = userClient.ImageV1().ImageSignatures().Create(&signature)
	if !kerrors.IsAlreadyExists(err) {
		t.Fatalf("expected already exists error, not: %v", err)
	}

	// try to create a signature with different name but the same conent
	newName, err := joinImageSignatureName(image.Name, "newone")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	signature.Name = newName
	created, err = userClient.ImageV1().ImageSignatures().Create(&signature)
	if !kerrors.IsAlreadyExists(err) {
		t.Fatalf("expected already exists error, not: %v", err)
	}

	// try to create a signature with the same name but different content
	signature.Name = sigName
	signature.Content = []byte("different")
	_, err = userClient.ImageV1().ImageSignatures().Create(&signature)
	if !kerrors.IsAlreadyExists(err) {
		t.Fatalf("expected already exists error, not: %v", err)
	}
}

func TestImageRemoveSignature(t *testing.T) {
	clusterAdminClientConfig, userKubeClient, _, userClient, image, fn := testSetupImageSignatureTest(t, testUserName)
	defer fn()
	makeUserAnImageSigner(rbacv1client.NewForConfigOrDie(clusterAdminClientConfig), userKubeClient, testUserName)

	// create some signatures
	sigData := []struct {
		sigName string
		content string
	}{
		{"a", "binaryblob"},
		{"b", "security without obscurity"},
		{"c", "distrust and caution are the parents of security"},
		{"d", "he who sacrifices freedom for security deserves neither"},
	}
	for i, d := range sigData {
		name, err := joinImageSignatureName(image.Name, d.sigName)
		if err != nil {
			t.Fatalf("creating signature %d: unexpected error: %v", i, err)
		}
		signature := imagev1.ImageSignature{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
			Type:    "unknown",
			Content: []byte(d.content),
		}
		_, err = userClient.ImageV1().ImageSignatures().Create(&signature)
		if err != nil {
			t.Fatalf("creating signature %d: unexpected error: %v", i, err)
		}
	}

	image, err := userClient.ImageV1().Images().Get(image.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(image.Signatures) != 4 {
		t.Fatalf("expected 4 signatures, not %d", len(image.Signatures))
	}

	// try to delete blob that does not exist
	err = userClient.ImageV1().ImageSignatures().Delete(image.Name+"@doesnotexist", nil)
	if !kerrors.IsNotFound(err) {
		t.Fatalf("expected not found error, not: %#+v", err)
	}

	// try to delete blob with missing signature name
	err = userClient.ImageV1().ImageSignatures().Delete(image.Name+"@", nil)
	if !kerrors.IsBadRequest(err) {
		t.Fatalf("expected bad request, not: %#+v", err)
	}

	// delete the first
	err = userClient.ImageV1().ImageSignatures().Delete(image.Name+"@"+sigData[0].sigName, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// try to delete it once more
	err = userClient.ImageV1().ImageSignatures().Delete(image.Name+"@"+sigData[0].sigName, nil)
	if err == nil {
		t.Fatalf("unexpected nont error")
	} else if !kerrors.IsNotFound(err) {
		t.Errorf("expected not found error, not: %#+v", err)
	}

	// delete the one in the middle
	err = userClient.ImageV1().ImageSignatures().Delete(image.Name+"@"+sigData[2].sigName, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if image, err = userClient.ImageV1().Images().Get(image.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	} else if len(image.Signatures) != 2 {
		t.Fatalf("expected 2 signatures, not %d", len(image.Signatures))
	}

	// delete the one at the end
	err = userClient.ImageV1().ImageSignatures().Delete(image.Name+"@"+sigData[3].sigName, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// delete the last one
	err = userClient.ImageV1().ImageSignatures().Delete(image.Name+"@"+sigData[1].sigName, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if image, err = userClient.ImageV1().Images().Get(image.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	} else if len(image.Signatures) != 0 {
		t.Fatalf("expected 2 signatures, not %d", len(image.Signatures))
	}
}

func testSetupImageSignatureTest(t *testing.T, userName string) (clusterAdminClientConfig *rest.Config, userKubeClient kubernetes.Interface, clusterAdminImageClient, userClient imagev1client.Interface, image *imagev1.Image, cleanup func()) {
	masterConfig, clusterAdminKubeConfig, err := testserver.StartTestMaster()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	clusterAdminConfig, err := testutil.GetClusterAdminClientConfig(clusterAdminKubeConfig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	clusterAdminImageClient = imagev1client.NewForConfigOrDie(clusterAdminConfig)

	image, err = getImageFixture("testdata/test-image.json")
	if err != nil {
		t.Fatalf("failed to read image fixture: %v", err)
	}

	image, err = clusterAdminImageClient.ImageV1().Images().Create(image)
	if err != nil {
		t.Fatalf("unexpected error creating image: %v", err)
	}

	if len(image.Signatures) != 0 {
		t.Fatalf("expected empty signatures, not: %s", diff.ObjectDiff(image.Signatures, []imagev1.ImageSignature{}))
	}

	var userConfig *rest.Config
	userKubeClient, userConfig, err = testutil.GetClientForUser(clusterAdminConfig, userName)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	return clusterAdminConfig, userKubeClient, clusterAdminImageClient, imagev1client.NewForConfigOrDie(userConfig), image, func() {
		testserver.CleanupMasterEtcd(t, masterConfig)
	}
}

func getImageFixture(filename string) (*imagev1.Image, error) {
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	obj, err := helpers.ReadYAML(bytes.NewBuffer(data), imagev1.Install)
	if err != nil {
		return nil, err
	}
	return obj.(*imagev1.Image), nil
}

func makeUserAnImageSigner(rbacClient rbacv1client.RbacV1Interface, userClient kubernetes.Interface, userName string) error {
	// give bob permissions to update image signatures
	addImageSignerRole := &policy.RoleModificationOptions{
		RoleName:   "system:image-signer",
		RoleKind:   "ClusterRole",
		RbacClient: rbacClient,
		Users:      []string{userName},
		PrintFlags: genericclioptions.NewPrintFlags(""),
		ToPrinter:  func(string) (printers.ResourcePrinter, error) { return printers.NewDiscardingPrinter(), nil },
	}
	if err := addImageSignerRole.AddRole(); err != nil {
		return err
	}
	return testutil.WaitForClusterPolicyUpdate(userClient.AuthorizationV1(), "create", corev1.Resource("imagesignatures"), true)
}

func compareSignatures(t *testing.T, a, b imagev1.ImageSignature) {
	aName := a.Name
	a.ObjectMeta = b.ObjectMeta
	a.Name = aName
	if !reflect.DeepEqual(a, b) {
		t.Errorf("created and contained signatures differ: %v", diff.ObjectDiff(a, b))
	}
}

// joinImageSignatureName joins image name and custom signature name into one string with @ separator.
func joinImageSignatureName(imageName, signatureName string) (string, error) {
	if len(imageName) == 0 {
		return "", fmt.Errorf("imageName may not be empty")
	}
	if len(signatureName) == 0 {
		return "", fmt.Errorf("signatureName may not be empty")
	}
	if strings.Count(imageName, "@") > 0 || strings.Count(signatureName, "@") > 0 {
		return "", fmt.Errorf("neither imageName nor signatureName can contain '@'")
	}
	return fmt.Sprintf("%s@%s", imageName, signatureName), nil
}
