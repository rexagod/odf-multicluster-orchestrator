package addons

import (
	"context"
	"fmt"
	"os"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/davecgh/go-spew/spew"
	noobaav1alpha1 "github.com/kube-object-storage/lib-bucket-provisioner/pkg/apis/objectbucket.io/v1alpha1"
	bktinformer "github.com/kube-object-storage/lib-bucket-provisioner/pkg/client/informers/externalversions/objectbucket.io/v1alpha1"
	bktlister "github.com/kube-object-storage/lib-bucket-provisioner/pkg/client/listers/objectbucket.io/v1alpha1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corev1lister "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

const (
	RookType                     = "kubernetes.io/rook"
	DefaultBlueSecretMatchString = "cluster-peer-token"
)

var (
	blueSecretMatchString string
)

type blueSecretTokenExchangeAgentController struct {
	hubKubeClient     kubernetes.Interface
	hubSecretLister   corev1lister.SecretLister
	spokeKubeClient   kubernetes.Interface
	spokeSecretLister corev1lister.SecretLister
	clusterName       string
	recorder          events.Recorder
	spokeKubeConfig   *rest.Config
	spokeBucketLister bktlister.ObjectBucketClaimLister
	customClient      client.Client
	bucketNamespace   *corev1.Namespace
	noobaaOBC         *noobaav1alpha1.ObjectBucketClaim
}

func newblueSecretTokenExchangeAgentController(
	hubKubeClient kubernetes.Interface,
	hubSecretInformers corev1informers.SecretInformer,
	spokeKubeClient kubernetes.Interface,
	spokeSecretInformers corev1informers.SecretInformer,
	clusterName string,
	recorder events.Recorder,
	spokeKubeConfig *rest.Config,
	spokeBucketInformers bktinformer.ObjectBucketClaimInformer,
	customClient client.Client,
	bucketNamespace *corev1.Namespace,
	noobaaOBC *noobaav1alpha1.ObjectBucketClaim,
) factory.Controller {
	c := &blueSecretTokenExchangeAgentController{
		hubKubeClient:     hubKubeClient,
		hubSecretLister:   hubSecretInformers.Lister(),
		spokeKubeClient:   spokeKubeClient,
		spokeSecretLister: spokeSecretInformers.Lister(),
		clusterName:       clusterName,
		recorder:          recorder,
		spokeKubeConfig:   spokeKubeConfig,
		spokeBucketLister: spokeBucketInformers.Lister(),
		customClient:      customClient,
		bucketNamespace:   bucketNamespace,
		noobaaOBC:         noobaaOBC,
	}
	klog.Infof("creating managed cluster to hub secret sync controller")

	blueSecretMatchString = os.Getenv("TOKEN_EXCHANGE_SOURCE_SECRET_STRING_MATCH")
	if blueSecretMatchString == "" {
		blueSecretMatchString = DefaultBlueSecretMatchString
	}

	queueKeyFn := func(obj runtime.Object) string {
		key, err := cache.MetaNamespaceKeyFunc(obj)
		if err != nil {
			return ""
		}
		return key
	}

	eventFilterFn := func(obj interface{}) bool {
		if obc, ok := obj.(*noobaav1alpha1.ObjectBucketClaim); ok {
			spew.Dump(obc)
			if obc.ObjectMeta.GenerateName == "odrbucket" && obc.ObjectMeta.DeletionTimestamp != nil {
				cl := c.customClient
				err := cl.Patch(context.TODO(), c.noobaaOBC, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`)))
				if err != nil {
					klog.Errorf("failed to remove finalizer from noobaa obc: %v", err)
					return false
				}
				return true
			}
		}
		return false
	}

	return factory.New().
		WithFilteredEventsInformersQueueKeyFunc(queueKeyFn, eventFilterFn, spokeBucketInformers.Informer()).
		WithSync(c.sync).
		ToController(fmt.Sprintf("managedcluster-secret-%s-controller", TokenExchangeName), recorder)
}

// sync is the main reconcile function that syncs secret from the managed cluster to the hub cluster
func (c *blueSecretTokenExchangeAgentController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	key := syncCtx.QueueKey()
	klog.Infof("reconciling addon deploy %q", key)

	// global bucket namespace
	c.bucketNamespace = &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{
			Kind: "namespace",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "openshift-dr-system",
		},
	}

	// global bucket
	c.noobaaOBC = &noobaav1alpha1.ObjectBucketClaim{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "odrbucket",
			Namespace:    "openshift-dr-system",
			Finalizers:   []string{"prevent-ob-deletion"},
		},
		Spec: noobaav1alpha1.ObjectBucketClaimSpec{
			StorageClassName: "openshift-storage.noobaa.io", // default storage class
		},
	}

	err := ensurebucketCreated(ctx, c.customClient, c.bucketNamespace, c.noobaaOBC)
	if err != nil {
		return err
	}

	return nil
}
