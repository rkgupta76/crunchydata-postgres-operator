// +build envtest

/*
 Copyright 2021 Crunchy Data Solutions, Inc.
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

package postgrescluster

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onsi/gomega"
	"github.com/onsi/gomega/gstruct"
	"go.opentelemetry.io/otel"
	"gotest.tools/v3/assert"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	"github.com/crunchydata/postgres-operator/internal/patroni"
	"github.com/crunchydata/postgres-operator/pkg/apis/postgres-operator.crunchydata.com/v1alpha1"
)

func TestReconcilerHandleDelete(t *testing.T) {
	if !strings.EqualFold(os.Getenv("USE_EXISTING_CLUSTER"), "true") {
		t.Skip("requires a running garbage collection controller")
	}
	// TODO: Update tests that include envtest package to better handle
	// running in parallel
	// t.Parallel()

	ctx := context.Background()
	env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "..", "config", "crd", "bases"),
		},
	}

	options := client.Options{}
	options.Scheme = runtime.NewScheme()
	assert.NilError(t, scheme.AddToScheme(options.Scheme))
	assert.NilError(t, v1alpha1.AddToScheme(options.Scheme))

	config, err := env.Start()
	assert.NilError(t, err)
	t.Cleanup(func() { assert.Check(t, env.Stop()) })

	cc, err := client.New(config, options)
	assert.NilError(t, err)

	ns := &v1.Namespace{}
	ns.GenerateName = "postgres-operator-test-"
	ns.Labels = labels.Set{"postgres-operator-test": t.Name()}
	assert.NilError(t, cc.Create(ctx, ns))
	t.Cleanup(func() { assert.Check(t, cc.Delete(ctx, ns)) })

	// TODO(cbandy): namespace rbac
	assert.NilError(t, cc.Create(ctx, &v1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns.Name, Name: "postgres-operator"},
	}))
	assert.NilError(t, cc.Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns.Name, Name: "postgres-operator"},
		RoleRef: rbacv1.RoleRef{
			Kind: "ClusterRole", Name: "postgres-operator-role",
		},
		Subjects: []rbacv1.Subject{{
			Kind: "ServiceAccount", Namespace: ns.Name, Name: "postgres-operator",
		}},
	}))

	reconciler := Reconciler{
		Client:   cc,
		Owner:    client.FieldOwner(t.Name()),
		Recorder: new(record.FakeRecorder),
		Tracer:   otel.Tracer(t.Name()),
	}

	reconciler.PodExec, err = newPodExecutor(config)
	assert.NilError(t, err)

	mustReconcile := func(t *testing.T, cluster *v1alpha1.PostgresCluster) reconcile.Result {
		t.Helper()
		key := client.ObjectKeyFromObject(cluster)
		request := reconcile.Request{NamespacedName: key}
		result, err := reconciler.Reconcile(ctx, request)
		assert.NilError(t, err, "%+v", err)
		return result
	}

	for _, test := range []struct {
		name         string
		beforeCreate func(*testing.T, *v1alpha1.PostgresCluster)
		beforeDelete func(*testing.T, *v1alpha1.PostgresCluster)
		propagation  metav1.DeletionPropagation

		waitForRunningInstances int32
	}{
		// Normal delete of a healthly cluster.
		{
			name: "Background", propagation: metav1.DeletePropagationBackground,
			waitForRunningInstances: 2,
		},
		// TODO(cbandy): metav1.DeletePropagationForeground

		// Normal delete of a healthy cluster after a failover.
		{
			name: "AfterFailover", propagation: metav1.DeletePropagationBackground,
			waitForRunningInstances: 2,

			beforeDelete: func(t *testing.T, cluster *v1alpha1.PostgresCluster) {
				list := v1.PodList{}
				selector, err := labels.Parse(strings.Join([]string{
					"postgres-operator.crunchydata.com/cluster=" + cluster.Name,
					"postgres-operator.crunchydata.com/instance",
				}, ","))
				assert.NilError(t, err)
				assert.NilError(t, cc.List(ctx, &list,
					client.InNamespace(cluster.Namespace),
					client.MatchingLabelsSelector{Selector: selector}))

				var primary *v1.Pod
				var replica *v1.Pod
				for i := range list.Items {
					if list.Items[i].Labels["postgres-operator.crunchydata.com/role"] == "replica" {
						replica = &list.Items[i]
					} else {
						primary = &list.Items[i]
					}
				}
				assert.Assert(t, primary != nil, "expected to find a primary in %+v", list.Items)
				assert.Assert(t, replica != nil, "expected to find a replica in %+v", list.Items)

				assert.NilError(t, patroni.Executor(func(_ context.Context, stdin io.Reader, stdout, stderr io.Writer, command ...string) error {
					return reconciler.PodExec(replica.Namespace, replica.Name, "database", stdin, stdout, stderr, command...)
				}).ChangePrimary(ctx, primary.Name, replica.Name))
			},
		},

		// Normal delete of cluster that could never run PostgreSQL.
		{
			name: "NeverRunning", propagation: metav1.DeletePropagationBackground,
			waitForRunningInstances: 0,

			beforeCreate: func(_ *testing.T, cluster *v1alpha1.PostgresCluster) {
				cluster.Spec.Image = "example.com/does-not-exist"
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			cluster := &v1alpha1.PostgresCluster{}
			assert.NilError(t, yaml.Unmarshal([]byte(`{
				spec: {
					postgresVersion: 12,
					instances: [
						{
							replicas: 2,
							volumeClaimSpec: {
								accessModes: [ReadWriteOnce],
								resources: { requests: { storage: 1Gi } },
							},
						},
					],
				},
			}`), cluster))

			cluster.Namespace = ns.Name
			cluster.Name = strings.ToLower(test.name)
			cluster.Spec.Image = CrunchyPostgresHAImage

			if test.beforeCreate != nil {
				test.beforeCreate(t, cluster)
			}

			assert.NilError(t, cc.Create(ctx, cluster))

			t.Cleanup(func() {
				// Remove finalizers, if any, so the namespace can terminate.
				assert.Check(t, client.IgnoreNotFound(
					cc.Patch(ctx, cluster, client.RawPatch(
						client.Merge.Type(), []byte(`{"metadata":{"finalizers":[]}}`)))))
			})

			// Start cluster.
			mustReconcile(t, cluster)

			assert.NilError(t,
				cc.Get(ctx, client.ObjectKeyFromObject(cluster), cluster))
			g.Expect(cluster.Finalizers).To(
				gomega.ContainElement("postgres-operator.crunchydata.com/finalizer"),
				"cluster should immediately have a finalizer")

			// Continue until instances are healthy.
			g.Eventually(func() (instances []appsv1.StatefulSet) {
				mustReconcile(t, cluster)

				list := appsv1.StatefulSetList{}
				selector, err := labels.Parse(strings.Join([]string{
					"postgres-operator.crunchydata.com/cluster=" + cluster.Name,
					"postgres-operator.crunchydata.com/instance",
				}, ","))
				assert.NilError(t, err)
				assert.NilError(t, cc.List(ctx, &list,
					client.InNamespace(cluster.Namespace),
					client.MatchingLabelsSelector{Selector: selector}))
				return list.Items
			},
				"60s", // timeout
				"1s",  // interval
			).Should(gomega.WithTransform(func(instances []appsv1.StatefulSet) int {
				ready := 0
				for _, sts := range instances {
					ready += int(sts.Status.ReadyReplicas)
				}
				return ready
			}, gomega.BeNumerically(">=", test.waitForRunningInstances)))

			if test.beforeDelete != nil {
				test.beforeDelete(t, cluster)
			}

			switch test.propagation {
			case metav1.DeletePropagationBackground:
				// Background deletion is the default for custom resources.
				// - https://issue.k8s.io/81628
				assert.NilError(t, cc.Delete(ctx, cluster))
			default:
				assert.NilError(t, cc.Delete(ctx, cluster,
					client.PropagationPolicy(test.propagation)))
			}

			// Stop cluster.
			result := mustReconcile(t, cluster)

			// If things started running, then they should stop in a certain order.
			if test.waitForRunningInstances > 0 {

				// Replicas should stop first, leaving just the one primary.
				g.Eventually(func() (instances []v1.Pod) {
					if result.Requeue {
						result = mustReconcile(t, cluster)
					}

					list := v1.PodList{}
					selector, err := labels.Parse(strings.Join([]string{
						"postgres-operator.crunchydata.com/cluster=" + cluster.Name,
						"postgres-operator.crunchydata.com/instance",
					}, ","))
					assert.NilError(t, err)
					assert.NilError(t, cc.List(ctx, &list,
						client.InNamespace(cluster.Namespace),
						client.MatchingLabelsSelector{Selector: selector}))
					return list.Items
				},
					"60s", // timeout
					"1s",  // interval
				).Should(gomega.ConsistOf(
					gstruct.MatchFields(gstruct.IgnoreExtras, gstruct.Fields{
						"ObjectMeta": gstruct.MatchFields(gstruct.IgnoreExtras, gstruct.Fields{
							"Labels": gstruct.MatchKeys(gstruct.IgnoreExtras, gstruct.Keys{
								// Patroni doesn't use "primary" to identify the primary.
								"postgres-operator.crunchydata.com/role": gomega.Equal("master"),
							}),
						}),
					}),
				), "expected one instance")

				// Patroni DCS objects should not be deleted yet.
				{
					list := v1.EndpointsList{}
					selector, err := labels.Parse(strings.Join([]string{
						"postgres-operator.crunchydata.com/cluster=" + cluster.Name,
						"postgres-operator.crunchydata.com/patroni",
					}, ","))
					assert.NilError(t, err)
					assert.NilError(t, cc.List(ctx, &list,
						client.InNamespace(cluster.Namespace),
						client.MatchingLabelsSelector{Selector: selector}))

					assert.Assert(t, len(list.Items) >= 2, // config + leader
						"expected Patroni DCS objects to remain, there are %v",
						len(list.Items))

					// Endpoints are deleted differently than other resources, and
					// Patroni might have recreated them to stay alive. Check that
					// they are all from before the cluster delete operation.
					// - https://issue.k8s.io/99407
					assert.NilError(t,
						cc.Get(ctx, client.ObjectKeyFromObject(cluster), cluster))
					g.Expect(list.Items).To(gstruct.MatchElements(
						func(interface{}) string { return "each" },
						gstruct.AllowDuplicates,
						gstruct.Elements{
							"each": gomega.WithTransform(
								func(ep v1.Endpoints) time.Time {
									return ep.CreationTimestamp.Time
								},
								gomega.BeTemporally("<", cluster.DeletionTimestamp.Time),
							),
						},
					))
				}
			}

			// Continue until cluster is gone.
			g.Eventually(func() error {
				mustReconcile(t, cluster)

				return cc.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)
			},
				"60s", // timeout
				"1s",  // interval
			).Should(gomega.SatisfyAll(
				gomega.HaveOccurred(),
				gomega.WithTransform(apierrors.IsNotFound, gomega.BeTrue()),
			))

			g.Eventually(func() []v1.Endpoints {
				list := v1.EndpointsList{}
				selector, err := labels.Parse(strings.Join([]string{
					"postgres-operator.crunchydata.com/cluster=" + cluster.Name,
					"postgres-operator.crunchydata.com/patroni",
				}, ","))
				assert.NilError(t, err)
				assert.NilError(t, cc.List(ctx, &list,
					client.InNamespace(cluster.Namespace),
					client.MatchingLabelsSelector{Selector: selector}))
				return list.Items
			},
				"20s", // timeout
				"1s",  // interval
			).Should(gomega.BeEmpty(), "Patroni DCS objects should be gone")
		})
	}
}

func TestReconcilerHandleDeleteNamespace(t *testing.T) {
	if !strings.EqualFold(os.Getenv("USE_EXISTING_CLUSTER"), "true") {
		t.Skip("requires a running garbage collection controller")
	}

	// TODO: Update tests that include envtest package to better handle
	// running in parallel
	// t.Parallel()

	ctx := context.Background()
	env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "..", "config", "crd", "bases"),
		},
	}

	options := client.Options{}
	options.Scheme = runtime.NewScheme()
	assert.NilError(t, scheme.AddToScheme(options.Scheme))
	assert.NilError(t, v1alpha1.AddToScheme(options.Scheme))

	config, err := env.Start()
	assert.NilError(t, err)
	t.Cleanup(func() { assert.Check(t, env.Stop()) })

	cc, err := client.New(config, options)
	assert.NilError(t, err)

	ns := &v1.Namespace{}
	ns.GenerateName = "postgres-operator-test-"
	ns.Labels = labels.Set{"postgres-operator-test": t.Name()}
	assert.NilError(t, cc.Create(ctx, ns))
	t.Cleanup(func() { assert.Check(t, client.IgnoreNotFound(cc.Delete(ctx, ns))) })

	// TODO(cbandy): namespace rbac
	assert.NilError(t, cc.Create(ctx, &v1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns.Name, Name: "postgres-operator"},
	}))
	assert.NilError(t, cc.Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns.Name, Name: "postgres-operator"},
		RoleRef: rbacv1.RoleRef{
			Kind: "ClusterRole", Name: "postgres-operator-role",
		},
		Subjects: []rbacv1.Subject{{
			Kind: "ServiceAccount", Namespace: ns.Name, Name: "postgres-operator",
		}},
	}))

	var mm struct {
		manager.Manager
		Context context.Context
		Error   chan error
		Stop    context.CancelFunc
	}

	mm.Context, mm.Stop = context.WithCancel(context.Background())
	mm.Error = make(chan error, 1)
	mm.Manager, err = manager.New(config, manager.Options{
		Namespace: ns.Name,
		Scheme:    options.Scheme,

		HealthProbeBindAddress: "0", // disable
		MetricsBindAddress:     "0", // disable
	})
	assert.NilError(t, err)

	reconciler := Reconciler{
		Client:   mm.GetClient(),
		Owner:    client.FieldOwner(t.Name()),
		Recorder: new(record.FakeRecorder),
		Tracer:   otel.Tracer(t.Name()),
	}
	assert.NilError(t, reconciler.SetupWithManager(mm.Manager))

	go func() { mm.Error <- mm.Start(mm.Context) }()
	t.Cleanup(func() { mm.Stop(); assert.Check(t, <-mm.Error) })

	cluster := &v1alpha1.PostgresCluster{}
	assert.NilError(t, yaml.Unmarshal([]byte(`{
		spec: {
			postgresVersion: 12,
			instances: [
				{
					replicas: 2,
					volumeClaimSpec: {
						accessModes: [ReadWriteOnce],
						resources: { requests: { storage: 1Gi } },
					},
				},
			],
		},
	}`), cluster))

	cluster.Namespace = ns.Name
	cluster.Name = strings.ToLower("DeleteNamespace")
	cluster.Spec.Image = CrunchyPostgresHAImage

	assert.NilError(t, cc.Create(ctx, cluster))

	t.Cleanup(func() {
		// Remove finalizers, if any, so the namespace can terminate.
		assert.Check(t, client.IgnoreNotFound(
			cc.Patch(ctx, cluster, client.RawPatch(
				client.Merge.Type(), []byte(`{"metadata":{"finalizers":[]}}`)))))
	})

	var instances []appsv1.StatefulSet
	assert.NilError(t, wait.Poll(time.Second, time.Minute, func() (bool, error) {
		list := appsv1.StatefulSetList{}
		selector, err := labels.Parse(strings.Join([]string{
			"postgres-operator.crunchydata.com/cluster=" + cluster.Name,
			"postgres-operator.crunchydata.com/instance",
		}, ","))
		assert.NilError(t, err)
		assert.NilError(t, cc.List(ctx, &list,
			client.InNamespace(cluster.Namespace),
			client.MatchingLabelsSelector{Selector: selector}))

		instances = list.Items

		ready := 0
		for i := range instances {
			ready += int(instances[i].Status.ReadyReplicas)
		}
		return ready >= 2, nil
	}), "expected instances to be ready, got:\n%+v", instances)

	// Delete the namespace.
	assert.NilError(t, cc.Delete(ctx, ns))

	assert.NilError(t, wait.PollImmediate(time.Second, time.Minute, func() (bool, error) {
		err := cc.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)
		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	}), "expected cluster to be deleted, got:\n%+v", *cluster)

	assert.NilError(t, wait.PollImmediate(time.Second, time.Minute/4, func() (bool, error) {
		err := cc.Get(ctx, client.ObjectKeyFromObject(ns), &v1.Namespace{})
		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	}), "expected namespace to be deleted")
}
