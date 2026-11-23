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

package controller_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2" //nolint:revive // Ginkgo DSL imports are the framework convention.
	. "github.com/onsi/gomega"    //nolint:revive // Gomega DSL imports are the framework convention.
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	koujouoanqounetv1 "koujouo.anqou.net/project/api/v1"
	"koujouo.anqou.net/project/internal/controller"
)

var _ = Describe("FactorioServer Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-resource"
			resourceNamespace = "default"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		factorioserver := &koujouoanqounetv1.FactorioServer{} //nolint:exhaustruct_v5 // Serves as the target of client Get.

		BeforeEach(func() {
			By("creating the custom resource for the Kind FactorioServer")

			err := k8sClient.Get(ctx, typeNamespacedName, factorioserver)
			if err != nil && errors.IsNotFound(err) {
				resource := &koujouoanqounetv1.FactorioServer{ //nolint:exhaustruct_v5 // Only the metadata matters for creation.
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: resourceNamespace,
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &koujouoanqounetv1.FactorioServer{} //nolint:exhaustruct_v5 // Serves as the target of client Get.
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance FactorioServer")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")

			controllerReconciler := &controller.FactorioServerReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
