package imagestreamtag

import (
	"context"
	"fmt"

	"github.com/openshift/origin/pkg/image/internalimageutil"

	"k8s.io/klog"

	kapierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternal "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/kubernetes/pkg/printers"
	printerstorage "k8s.io/kubernetes/pkg/printers/storage"

	imagegroup "github.com/openshift/api/image"
	"github.com/openshift/library-go/pkg/image/imageutil"
	"github.com/openshift/origin/pkg/api/apihelpers"
	imageapi "github.com/openshift/origin/pkg/image/apis/image"
	"github.com/openshift/origin/pkg/image/apis/image/validation/whitelist"
	"github.com/openshift/origin/pkg/image/apiserver/registry/image"
	"github.com/openshift/origin/pkg/image/apiserver/registry/imagestream"
	printersinternal "github.com/openshift/origin/pkg/printers/internalversion"
)

// REST implements the RESTStorage interface for ImageStreamTag
// It only supports the Get method and is used to simplify retrieving an Image by tag from an ImageStream
type REST struct {
	imageRegistry       image.Registry
	imageStreamRegistry imagestream.Registry
	strategy            Strategy
	rest.TableConvertor
}

// NewREST returns a new REST.
func NewREST(imageRegistry image.Registry, imageStreamRegistry imagestream.Registry, registryWhitelister whitelist.RegistryWhitelister) *REST {
	return &REST{
		imageRegistry:       imageRegistry,
		imageStreamRegistry: imageStreamRegistry,
		strategy:            NewStrategy(registryWhitelister),
		TableConvertor:      printerstorage.TableConvertor{TablePrinter: printers.NewTablePrinter().With(printersinternal.AddHandlers)},
	}
}

var _ rest.Getter = &REST{}
var _ rest.Lister = &REST{}
var _ rest.CreaterUpdater = &REST{}
var _ rest.GracefulDeleter = &REST{}
var _ rest.ShortNamesProvider = &REST{}
var _ rest.Scoper = &REST{}

// ShortNames implements the ShortNamesProvider interface. Returns a list of short names for a resource.
func (r *REST) ShortNames() []string {
	return []string{"istag"}
}

// New is only implemented to make REST implement RESTStorage
func (r *REST) New() runtime.Object {
	return &imageapi.ImageStreamTag{}
}

// NewList returns a new list object
func (r *REST) NewList() runtime.Object {
	return &imageapi.ImageStreamTagList{}
}

func (s *REST) NamespaceScoped() bool {
	return true
}

// nameAndTag splits a string into its name component and tag component, and returns an error
// if the string is not in the right form.
func nameAndTag(id string) (name string, tag string, err error) {
	name, tag, err = imageutil.ParseImageStreamTagName(id)
	if err != nil {
		err = kapierrors.NewBadRequest("ImageStreamTags must be retrieved with <name>:<tag>")
	}
	return
}

func (r *REST) List(ctx context.Context, options *metainternal.ListOptions) (runtime.Object, error) {
	imageStreams, err := r.imageStreamRegistry.ListImageStreams(ctx, options)
	if err != nil {
		return nil, err
	}

	matcher := MatchImageStreamTag(apihelpers.InternalListOptionsToSelectors(options))

	list := &imageapi.ImageStreamTagList{}
	for _, currIS := range imageStreams.Items {
		for currTag := range currIS.Status.Tags {
			istag, err := newISTag(currTag, &currIS, nil, false)
			if err != nil {
				if kapierrors.IsNotFound(err) {
					continue
				}
				return nil, err
			}
			matches, err := matcher.Matches(istag)
			if err != nil {
				return nil, err
			}

			if matches {
				list.Items = append(list.Items, *istag)
			}
		}
	}

	return list, nil
}

// Get retrieves an image that has been tagged by stream and tag. `id` is of the format <stream name>:<tag>.
func (r *REST) Get(ctx context.Context, id string, options *metav1.GetOptions) (runtime.Object, error) {
	name, tag, err := nameAndTag(id)
	if err != nil {
		return nil, err
	}

	imageStream, err := r.imageStreamRegistry.GetImageStream(ctx, name, options)
	if err != nil {
		return nil, err
	}

	image, err := r.imageFor(ctx, tag, imageStream)
	if err != nil {
		return nil, err
	}

	return newISTag(tag, imageStream, image, false)
}

func (r *REST) Create(ctx context.Context, obj runtime.Object, createValidation rest.ValidateObjectFunc, options *metav1.CreateOptions) (runtime.Object, error) {
	istag, ok := obj.(*imageapi.ImageStreamTag)
	if !ok {
		return nil, kapierrors.NewBadRequest(fmt.Sprintf("obj is not an ImageStreamTag: %#v", obj))
	}
	if err := rest.BeforeCreate(r.strategy, ctx, obj); err != nil {
		return nil, err
	}
	if err := createValidation(obj.DeepCopyObject()); err != nil {
		return nil, err
	}
	namespace, ok := apirequest.NamespaceFrom(ctx)
	if !ok {
		return nil, kapierrors.NewBadRequest("a namespace must be specified to import images")
	}

	imageStreamName, imageTag, ok := imageutil.SplitImageStreamTag(istag.Name)
	if !ok {
		return nil, fmt.Errorf("%q must be of the form <stream_name>:<tag>", istag.Name)
	}

	for i := 10; i > 0; i-- {
		target, err := r.imageStreamRegistry.GetImageStream(ctx, imageStreamName, &metav1.GetOptions{})
		if err != nil {
			if !kapierrors.IsNotFound(err) {
				return nil, err
			}

			// try to create the target if it doesn't exist
			target = &imageapi.ImageStream{
				ObjectMeta: metav1.ObjectMeta{
					Name:      imageStreamName,
					Namespace: namespace,
				},
			}
		}

		if target.Spec.Tags == nil {
			target.Spec.Tags = make(map[string]imageapi.TagReference)
		}

		// The user wants to symlink a tag.
		_, exists := target.Spec.Tags[imageTag]
		if exists {
			return nil, kapierrors.NewAlreadyExists(imagegroup.Resource("imagestreamtag"), istag.Name)
		}
		if istag.Tag != nil {
			target.Spec.Tags[imageTag] = *istag.Tag
		}

		// Check the stream creation timestamp and make sure we will not
		// create a new image stream while deleting.
		if target.CreationTimestamp.IsZero() {
			target, err = r.imageStreamRegistry.CreateImageStream(ctx, target, &metav1.CreateOptions{})
		} else {
			target, err = r.imageStreamRegistry.UpdateImageStream(ctx, target, false, &metav1.UpdateOptions{})
		}
		if kapierrors.IsAlreadyExists(err) || kapierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		image, _ := r.imageFor(ctx, imageTag, target)
		return newISTag(imageTag, target, image, true)
	}
	// We tried to update resource, but we kept conflicting. Inform the client that we couldn't complete
	// the operation but that they may try again.
	return nil, kapierrors.NewServerTimeout(imagegroup.Resource("imagestreamtags"), "create", 2)
}

func (r *REST) Update(ctx context.Context, tagName string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, forceAllowCreate bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	name, tag, err := nameAndTag(tagName)
	if err != nil {
		return nil, false, err
	}

	create := false
	imageStream, err := r.imageStreamRegistry.GetImageStream(ctx, name, &metav1.GetOptions{})
	if err != nil {
		if !kapierrors.IsNotFound(err) {
			return nil, false, err
		}
		namespace, ok := apirequest.NamespaceFrom(ctx)
		if !ok {
			return nil, false, kapierrors.NewBadRequest("namespace is required on ImageStreamTags")
		}
		imageStream = &imageapi.ImageStream{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace,
				Name:      name,
			},
		}
		rest.FillObjectMetaSystemFields(&imageStream.ObjectMeta)
		create = true
	}

	// create the synthetic old istag
	old, err := newISTag(tag, imageStream, nil, true)
	if err != nil {
		return nil, false, err
	}

	obj, err := objInfo.UpdatedObject(ctx, old)
	if err != nil {
		return nil, false, err
	}

	istag, ok := obj.(*imageapi.ImageStreamTag)
	if !ok {
		return nil, false, kapierrors.NewBadRequest(fmt.Sprintf("obj is not an ImageStreamTag: %#v", obj))
	}

	// check for conflict
	switch {
	case len(istag.ResourceVersion) == 0:
		// should disallow blind PUT, but this was previously supported
		istag.ResourceVersion = imageStream.ResourceVersion
	case len(imageStream.ResourceVersion) == 0:
		// image stream did not exist, cannot update
		return nil, false, kapierrors.NewNotFound(imagegroup.Resource("imagestreamtags"), tagName)
	case imageStream.ResourceVersion != istag.ResourceVersion:
		// conflicting input and output
		return nil, false, kapierrors.NewConflict(imagegroup.Resource("imagestreamtags"), istag.Name, fmt.Errorf("another caller has updated the resource version to %s", imageStream.ResourceVersion))
	}

	// When we began returning image stream labels in 3.6, old clients that didn't need to send labels would be
	// broken on update. Explicitly default labels if they are unset.  We don't support mutation of labels on update.
	if len(imageStream.Labels) > 0 && len(istag.Labels) == 0 {
		istag.Labels = imageStream.Labels
	}

	if create {
		if err := rest.BeforeCreate(r.strategy, ctx, obj); err != nil {
			return nil, false, err
		}
		if err := createValidation(obj.DeepCopyObject()); err != nil {
			return nil, false, err
		}
	} else {
		if err := rest.BeforeUpdate(r.strategy, ctx, obj, old); err != nil {
			return nil, false, err
		}
		if err := updateValidation(obj.DeepCopyObject(), old.DeepCopyObject()); err != nil {
			return nil, false, err
		}
	}

	// update the spec tag
	if imageStream.Spec.Tags == nil {
		imageStream.Spec.Tags = map[string]imageapi.TagReference{}
	}
	tagRef, exists := imageStream.Spec.Tags[tag]

	if !exists && istag.Tag == nil {
		return nil, false, kapierrors.NewBadRequest(fmt.Sprintf("imagestreamtag %s is not a spec tag in imagestream %s/%s, cannot be updated", tag, imageStream.Namespace, imageStream.Name))
	}

	// if the caller set tag, override the spec tag
	if istag.Tag != nil {
		tagRef = *istag.Tag
		tagRef.Name = tag
	}
	tagRef.Annotations = istag.Annotations
	imageStream.Spec.Tags[tag] = tagRef

	// mutate the image stream
	var newImageStream *imageapi.ImageStream
	if create {
		newImageStream, err = r.imageStreamRegistry.CreateImageStream(ctx, imageStream, &metav1.CreateOptions{})
	} else {
		newImageStream, err = r.imageStreamRegistry.UpdateImageStream(ctx, imageStream, false, &metav1.UpdateOptions{})
	}
	if err != nil {
		return nil, false, err
	}

	image, err := r.imageFor(ctx, tag, newImageStream)
	if err != nil {
		if !kapierrors.IsNotFound(err) {
			return nil, false, err
		}
	}

	newISTag, err := newISTag(tag, newImageStream, image, true)
	return newISTag, !exists, err
}

// Delete removes a tag from a stream. `id` is of the format <stream name>:<tag>.
// The associated image that the tag points to is *not* deleted.
// The tag history is removed.
func (r *REST) Delete(ctx context.Context, id string, options *metav1.DeleteOptions) (runtime.Object, bool, error) {
	name, tag, err := nameAndTag(id)
	if err != nil {
		return nil, false, err
	}

	for i := 10; i > 0; i-- {
		stream, err := r.imageStreamRegistry.GetImageStream(ctx, name, &metav1.GetOptions{})
		if err != nil {
			return nil, false, err
		}
		if options != nil {
			if pre := options.Preconditions; pre != nil {
				if pre.UID != nil && *pre.UID != stream.UID {
					return nil, false, kapierrors.NewConflict(imagegroup.Resource("imagestreamtags"), id, fmt.Errorf("the UID precondition was not met"))
				}
			}
		}

		notFound := true

		// Try to delete the status tag
		if _, ok := stream.Status.Tags[tag]; ok {
			delete(stream.Status.Tags, tag)
			notFound = false
		}

		// Try to delete the spec tag
		if _, ok := stream.Spec.Tags[tag]; ok {
			delete(stream.Spec.Tags, tag)
			notFound = false
		}

		if notFound {
			return nil, false, kapierrors.NewNotFound(imagegroup.Resource("imagestreamtags"), id)
		}

		_, err = r.imageStreamRegistry.UpdateImageStream(ctx, stream, false, &metav1.UpdateOptions{})
		if kapierrors.IsConflict(err) {
			continue
		}
		if err != nil && !kapierrors.IsNotFound(err) {
			return nil, false, err
		}
		return &metav1.Status{Status: metav1.StatusSuccess}, true, nil
	}
	// We tried to update resource, but we kept conflicting. Inform the client that we couldn't complete
	// the operation but that they may try again.
	return nil, false, kapierrors.NewServerTimeout(imagegroup.Resource("imagestreamtags"), "delete", 2)
}

// imageFor retrieves the most recent image for a tag in a given imageStreem.
func (r *REST) imageFor(ctx context.Context, tag string, imageStream *imageapi.ImageStream) (*imageapi.Image, error) {
	event := internalimageutil.LatestTaggedImage(imageStream, tag)
	if event == nil || len(event.Image) == 0 {
		return nil, kapierrors.NewNotFound(imagegroup.Resource("imagestreamtags"), imageutil.JoinImageStreamTag(imageStream.Name, tag))
	}

	return r.imageRegistry.GetImage(ctx, event.Image, &metav1.GetOptions{})
}

// newISTag initializes an image stream tag from an image stream and image. The allowEmptyEvent will create a tag even
// in the event that the status tag does does not exist yet (no image has successfully been tagged) or the image is nil.
func newISTag(tag string, imageStream *imageapi.ImageStream, image *imageapi.Image, allowEmptyEvent bool) (*imageapi.ImageStreamTag, error) {
	istagName := imageutil.JoinImageStreamTag(imageStream.Name, tag)

	event := internalimageutil.LatestTaggedImage(imageStream, tag)
	if event == nil || len(event.Image) == 0 {
		if !allowEmptyEvent {
			klog.V(4).Infof("did not find tag %s in image stream status tags: %#v", tag, imageStream.Status.Tags)
			return nil, kapierrors.NewNotFound(imagegroup.Resource("imagestreamtags"), istagName)
		}
		event = &imageapi.TagEvent{
			Created: imageStream.CreationTimestamp,
		}
	}

	ist := &imageapi.ImageStreamTag{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         imageStream.Namespace,
			Name:              istagName,
			CreationTimestamp: event.Created,
			Annotations:       map[string]string{},
			Labels:            imageStream.Labels,
			ResourceVersion:   imageStream.ResourceVersion,
			UID:               imageStream.UID,
		},
		Generation: event.Generation,
		Conditions: imageStream.Status.Tags[tag].Conditions,

		LookupPolicy: imageStream.Spec.LookupPolicy,
	}

	if imageStream.Spec.Tags != nil {
		if tagRef, ok := imageStream.Spec.Tags[tag]; ok {
			// copy the spec tag
			ist.Tag = &tagRef
			if from := ist.Tag.From; from != nil {
				copied := *from
				ist.Tag.From = &copied
			}
			if gen := ist.Tag.Generation; gen != nil {
				copied := *gen
				ist.Tag.Generation = &copied
			}

			// if the imageStream has Spec.Tags[tag].Annotations[k] = v, copy it to the image's annotations
			// and add them to the istag's annotations
			if image != nil && image.Annotations == nil {
				image.Annotations = make(map[string]string)
			}
			for k, v := range tagRef.Annotations {
				ist.Annotations[k] = v
				if image != nil {
					image.Annotations[k] = v
				}
			}
		}
	}

	if image != nil {
		if err := internalimageutil.InternalImageWithMetadata(image); err != nil {
			return nil, err
		}
		image.DockerImageManifest = ""
		image.DockerImageConfig = ""
		ist.Image = *image
	} else {
		ist.Image = imageapi.Image{}
		ist.Image.Name = event.Image
	}

	ist.Image.DockerImageReference = internalimageutil.ResolveReferenceForTagEvent(imageStream, tag, event)
	return ist, nil
}
