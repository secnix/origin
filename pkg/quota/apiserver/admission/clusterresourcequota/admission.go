package clusterresourcequota

import (
	"errors"
	"io"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/admission/initializer"
	"k8s.io/apiserver/pkg/admission/plugin/namespace/lifecycle"
	"k8s.io/client-go/informers"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	quota "k8s.io/kubernetes/pkg/quota/v1"
	"k8s.io/kubernetes/pkg/quota/v1/install"
	"k8s.io/kubernetes/plugin/pkg/admission/resourcequota"
	resourcequotaapi "k8s.io/kubernetes/plugin/pkg/admission/resourcequota/apis/resourcequota"

	quotatypedclient "github.com/openshift/client-go/quota/clientset/versioned/typed/quota/v1"
	quotainformer "github.com/openshift/client-go/quota/informers/externalversions/quota/v1"
	quotalister "github.com/openshift/client-go/quota/listers/quota/v1"
	"github.com/openshift/library-go/pkg/quota/clusterquotamapping"
	oadmission "github.com/openshift/origin/pkg/cmd/server/admission"
)

func Register(plugins *admission.Plugins) {
	plugins.Register("quota.openshift.io/ClusterResourceQuota",
		func(config io.Reader) (admission.Interface, error) {
			return NewClusterResourceQuota()
		})
}

// clusterQuotaAdmission implements an admission controller that can enforce clusterQuota constraints
type clusterQuotaAdmission struct {
	*admission.Handler

	// these are used to create the accessor
	clusterQuotaLister quotalister.ClusterResourceQuotaLister
	namespaceLister    corev1listers.NamespaceLister
	clusterQuotaSynced func() bool
	namespaceSynced    func() bool
	clusterQuotaClient quotatypedclient.ClusterResourceQuotasGetter
	clusterQuotaMapper clusterquotamapping.ClusterQuotaMapper

	lockFactory LockFactory

	// these are used to create the evaluator
	registry quota.Registry

	init      sync.Once
	evaluator resourcequota.Evaluator
}

var _ initializer.WantsExternalKubeInformerFactory = &clusterQuotaAdmission{}
var _ oadmission.WantsRESTClientConfig = &clusterQuotaAdmission{}
var _ oadmission.WantsClusterQuota = &clusterQuotaAdmission{}
var _ admission.ValidationInterface = &clusterQuotaAdmission{}

const (
	timeToWaitForCacheSync = 10 * time.Second
	numEvaluatorThreads    = 10
)

// NewClusterResourceQuota configures an admission controller that can enforce clusterQuota constraints
// using the provided registry.  The registry must have the capability to handle group/kinds that
// are persisted by the server this admission controller is intercepting
func NewClusterResourceQuota() (admission.Interface, error) {
	return &clusterQuotaAdmission{
		Handler:     admission.NewHandler(admission.Create, admission.Update),
		lockFactory: NewDefaultLockFactory(),
	}, nil
}

// Admit makes admission decisions while enforcing clusterQuota
func (q *clusterQuotaAdmission) Validate(a admission.Attributes, _ admission.ObjectInterfaces) (err error) {
	// ignore all operations that correspond to sub-resource actions
	if len(a.GetSubresource()) != 0 {
		return nil
	}
	// ignore cluster level resources
	if len(a.GetNamespace()) == 0 {
		return nil
	}

	if !q.waitForSyncedStore(time.After(timeToWaitForCacheSync)) {
		return admission.NewForbidden(a, errors.New("caches not synchronized"))
	}

	q.init.Do(func() {
		clusterQuotaAccessor := newQuotaAccessor(q.clusterQuotaLister, q.namespaceLister, q.clusterQuotaClient, q.clusterQuotaMapper)
		q.evaluator = resourcequota.NewQuotaEvaluator(clusterQuotaAccessor, ignoredResources, q.registry, q.lockAquisition, &resourcequotaapi.Configuration{}, numEvaluatorThreads, utilwait.NeverStop)
	})

	return q.evaluator.Evaluate(a)
}

func (q *clusterQuotaAdmission) lockAquisition(quotas []corev1.ResourceQuota) func() {
	locks := []sync.Locker{}

	// acquire the locks in alphabetical order because I'm too lazy to think of something clever
	sort.Sort(ByName(quotas))
	for _, quota := range quotas {
		lock := q.lockFactory.GetLock(quota.Name)
		lock.Lock()
		locks = append(locks, lock)
	}

	return func() {
		for i := len(locks) - 1; i >= 0; i-- {
			locks[i].Unlock()
		}
	}
}

func (q *clusterQuotaAdmission) waitForSyncedStore(timeout <-chan time.Time) bool {
	for !q.clusterQuotaSynced() || !q.namespaceSynced() {
		select {
		case <-time.After(100 * time.Millisecond):
		case <-timeout:
			return q.clusterQuotaSynced() && q.namespaceSynced()
		}
	}

	return true
}

func (q *clusterQuotaAdmission) SetOriginQuotaRegistry(registry quota.Registry) {
	q.registry = registry
}

func (q *clusterQuotaAdmission) SetExternalKubeInformerFactory(informers informers.SharedInformerFactory) {
	q.namespaceLister = informers.Core().V1().Namespaces().Lister()
	q.namespaceSynced = informers.Core().V1().Namespaces().Informer().HasSynced
}

func (q *clusterQuotaAdmission) SetRESTClientConfig(restClientConfig rest.Config) {
	var err error

	// ClusterResourceQuota is served using CRD resource any status update must use JSON
	jsonClientConfig := rest.CopyConfig(&restClientConfig)
	jsonClientConfig.ContentConfig.AcceptContentTypes = "application/json"
	jsonClientConfig.ContentConfig.ContentType = "application/json"

	q.clusterQuotaClient, err = quotatypedclient.NewForConfig(jsonClientConfig)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
}

func (q *clusterQuotaAdmission) SetClusterQuota(clusterQuotaMapper clusterquotamapping.ClusterQuotaMapper, informers quotainformer.ClusterResourceQuotaInformer) {
	q.clusterQuotaMapper = clusterQuotaMapper
	q.clusterQuotaLister = informers.Lister()
	q.clusterQuotaSynced = informers.Informer().HasSynced
}

func (q *clusterQuotaAdmission) ValidateInitialization() error {
	if q.clusterQuotaLister == nil {
		return errors.New("missing clusterQuotaLister")
	}
	if q.namespaceLister == nil {
		return errors.New("missing namespaceLister")
	}
	if q.clusterQuotaClient == nil {
		return errors.New("missing clusterQuotaClient")
	}
	if q.clusterQuotaMapper == nil {
		return errors.New("missing clusterQuotaMapper")
	}
	if q.registry == nil {
		return errors.New("missing registry")
	}

	return nil
}

type ByName []corev1.ResourceQuota

func (v ByName) Len() int           { return len(v) }
func (v ByName) Swap(i, j int)      { v[i], v[j] = v[j], v[i] }
func (v ByName) Less(i, j int) bool { return v[i].Name < v[j].Name }

// ignoredResources is the set of resources that clusterquota ignores.  It's larger because we have to ignore requests
// that the namespace lifecycle plugin ignores.  This is because of the need to have a matching namespace in order to be sure
// that the cache is current enough to have mapped the CRQ to the namespaces.  Normal RQ doesn't have that requirement.
var ignoredResources = map[schema.GroupResource]struct{}{}

func init() {
	for k := range install.DefaultIgnoredResources() {
		ignoredResources[k] = struct{}{}
	}
	for k := range lifecycle.AccessReviewResources() {
		ignoredResources[k] = struct{}{}
	}

}
