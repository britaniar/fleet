/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT license.
*/

package workapplier

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/textlogger"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	fleetv1beta1 "go.goms.io/fleet/apis/placement/v1beta1"
	testv1alpha1 "go.goms.io/fleet/test/apis/v1alpha1"
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.
var (
	hubCfg              *rest.Config
	memberCfg           *rest.Config
	hubEnv              *envtest.Environment
	memberEnv           *envtest.Environment
	hubMgr              manager.Manager
	hubClient           client.Client
	memberClient        client.Client
	memberDynamicClient dynamic.Interface
	workApplier         *Reconciler

	ctx    context.Context
	cancel context.CancelFunc

	// Temporary variables for migrated integration tests.
	tmpEnv         *envtest.Environment
	tmpCfg         *rest.Config
	k8sClient      client.Client
	tmpMgr         manager.Manager
	workController *Reconciler

	testWorkNamespace = "test-work-namespace"
)

const (
	// The number of max. concurrent reconciliations for the work applier controller.
	maxConcurrentReconciles = 5
	// The count of workers for the work applier controller.
	workerCount = 4

	memberReservedNSName = "fleet-member-experimental"
)

func TestAPIs(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Work Applier Integration Test Suite")
}

func setupResources() {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: memberReservedNSName,
		},
	}
	Expect(hubClient.Create(ctx, ns)).To(Succeed())
}

var _ = BeforeSuite(func() {
	ctx, cancel = context.WithCancel(context.TODO())

	By("Setup klog")
	fs := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(fs)
	Expect(fs.Parse([]string{"--v", "5", "-add_dir_header", "true"})).Should(Succeed())

	klog.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	By("Bootstrapping test environments")
	hubEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("../../../", "config", "crd", "bases"),
			filepath.Join("../../../", "test", "manifests"),
		},
	}
	memberEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("../../../", "config", "crd", "bases"),
			filepath.Join("../../../", "test", "manifests"),
		},
	}

	var err error
	hubCfg, err = hubEnv.Start()
	Expect(err).ToNot(HaveOccurred())
	Expect(hubCfg).ToNot(BeNil())

	memberCfg, err = memberEnv.Start()
	Expect(err).ToNot(HaveOccurred())
	Expect(memberCfg).ToNot(BeNil())

	err = fleetv1beta1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())
	err = testv1alpha1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	By("Building the K8s clients")
	hubClient, err = client.New(hubCfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).ToNot(HaveOccurred())
	Expect(hubClient).ToNot(BeNil())

	memberClient, err = client.New(memberCfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).ToNot(HaveOccurred())
	Expect(memberClient).ToNot(BeNil())

	// This setup also requires a client-go dynamic client for the member cluster.
	memberDynamicClient, err = dynamic.NewForConfig(memberCfg)
	Expect(err).ToNot(HaveOccurred())

	By("Setting up the resources")
	setupResources()

	By("Setting up the controller and the controller manager")
	hubMgr, err = ctrl.NewManager(hubCfg, ctrl.Options{
		Scheme: scheme.Scheme,
		Metrics: server.Options{
			BindAddress: "0",
		},
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				memberReservedNSName: {},
			},
		},
		Logger: textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(4))),
	})
	Expect(err).ToNot(HaveOccurred())

	workApplier = NewReconciler(
		hubClient,
		memberReservedNSName,
		memberDynamicClient,
		memberClient,
		memberClient.RESTMapper(),
		hubMgr.GetEventRecorderFor("work-applier"),
		maxConcurrentReconciles,
		workerCount,
		time.Second*5,
		time.Second*5,
	)
	Expect(workApplier.SetupWithManager(hubMgr)).To(Succeed())

	go func() {
		defer GinkgoRecover()
		Expect(workApplier.Join(ctx)).To(Succeed())
		Expect(hubMgr.Start(ctx)).To(Succeed())
	}()

	// Temporary setup for migrated integration tests.
	tmpEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("../../../", "config", "crd", "bases"),
			filepath.Join("../../../", "test", "manifests"),
		},
	}

	tmpCfg, err = tmpEnv.Start()
	Expect(err).ToNot(HaveOccurred())
	Expect(tmpCfg).ToNot(BeNil())

	k8sClient, err = client.New(tmpCfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).ToNot(HaveOccurred())

	workNamespace := corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: testWorkNamespace,
		},
	}
	err = k8sClient.Create(context.Background(), &workNamespace)
	Expect(err).ToNot(HaveOccurred())

	tmpMgr, err = ctrl.NewManager(tmpCfg, ctrl.Options{
		Scheme: scheme.Scheme,
		Metrics: server.Options{
			BindAddress: "0",
		},
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				testWorkNamespace: {},
			},
		},
		Logger: textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(4))),
	})
	Expect(err).ToNot(HaveOccurred())

	tmpSpokeDynamicClient, err := dynamic.NewForConfig(tmpCfg)
	Expect(err).ToNot(HaveOccurred())

	tmpSpokeClient, err := client.New(tmpCfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).ToNot(HaveOccurred())

	workController = NewReconciler(
		tmpMgr.GetClient(),
		testWorkNamespace,
		tmpSpokeDynamicClient,
		tmpSpokeClient,
		tmpSpokeClient.RESTMapper(),
		tmpMgr.GetEventRecorderFor("work-applier"),
		maxConcurrentReconciles,
		workerCount,
		time.Second*5,
		time.Second*5,
	)
	Expect(workController.SetupWithManager(tmpMgr)).To(Succeed())
	Expect(workController.Join(ctx)).To(Succeed())

	go func() {
		if err = tmpMgr.Start(ctx); err != nil {
			os.Exit(1)
		}
		Expect(err).ToNot(HaveOccurred())
	}()
})

var _ = AfterSuite(func() {
	defer klog.Flush()

	cancel()
	By("Tearing down the test environment")
	Expect(hubEnv.Stop()).To(Succeed())
	Expect(memberEnv.Stop()).To(Succeed())

	// Temporary setup for migrated integration tests.
	Expect(tmpEnv.Stop()).To(Succeed())
})