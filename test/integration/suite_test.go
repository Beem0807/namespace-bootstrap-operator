/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package integration runs the operator the way it actually runs: a real
// controller-runtime manager, with its cache, informers, work queue and
// watches, against a real API server.
//
// It is deliberately separate from the specs in internal/controller, which
// call Reconcile directly. Those are precise — the sequence of reconciliations
// is explicit, so they can assert that a given pass performed exactly zero
// writes. They cannot, however, tell you whether the controller would ever
// have been woken in the first place.
//
// That is what these specs cover, and it is the half that matters for drift:
// the repair logic being correct is useless if nothing triggers it. Nothing
// here calls Reconcile. Each spec changes the cluster and then waits, so a
// missing or misconfigured watch shows up as a timeout rather than silently
// passing.
package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	platformv1alpha1 "github.com/Beem0807/namespace-bootstrap-operator/api/v1alpha1"
	"github.com/Beem0807/namespace-bootstrap-operator/internal/controller"
)

var (
	ctx     context.Context
	cancel  context.CancelFunc
	testEnv *envtest.Environment

	// k8sClient reads and writes directly against the API server, bypassing
	// the manager's cache.
	//
	// Assertions must not go through the cached client: a spec that asserted
	// against the same cache the controller reads could pass on a stale copy,
	// or fail only because an event had not been delivered yet. Reading
	// directly means every assertion is about what the cluster actually
	// contains.
	k8sClient client.Client
)

func TestIntegration(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Integration Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	// Everything here waits on a running controller, so an assertion that
	// never comes true should fail in seconds rather than hanging.
	SetDefaultEventuallyTimeout(15 * time.Second)
	SetDefaultEventuallyPollingInterval(100 * time.Millisecond)
	SetDefaultConsistentlyDuration(2 * time.Second)

	ctx, cancel = context.WithCancel(context.TODO())

	Expect(platformv1alpha1.AddToScheme(scheme.Scheme)).To(Succeed())

	By("bootstrapping the test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	if dir := firstEnvtestBinaryDir(); dir != "" {
		testEnv.BinaryAssetsDirectory = dir
	}

	cfg, err := testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())

	By("starting a manager with the real controller")
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme.Scheme,
		// Nothing here scrapes metrics or probes health, and binding real
		// ports would make the suite fail when run concurrently with anything
		// else.
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	Expect(err).NotTo(HaveOccurred())

	Expect((&controller.NamespaceProfileReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("namespaceprofile-controller"),
	}).SetupWithManager(mgr)).To(Succeed())

	go func() {
		defer GinkgoRecover()
		Expect(mgr.Start(ctx)).To(Succeed(), "the manager stopped unexpectedly")
	}()

	// Without this, the first spec could create a profile before the informers
	// had listed, and the controller would miss the event — a flake that looks
	// exactly like a missing watch.
	Expect(mgr.GetCache().WaitForCacheSync(ctx)).To(BeTrue())
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	Expect(testEnv.Stop()).To(Succeed())
})

// firstEnvtestBinaryDir locates the envtest binaries when the suite is run
// outside the Makefile, which is what an IDE's test runner does. Run
// `make setup-envtest` first.
func firstEnvtestBinaryDir() string {
	base := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(base, entry.Name())
		}
	}
	return ""
}
