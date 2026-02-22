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

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	claudetownv1alpha1 "github.com/marcoscandeia/claude-town/api/v1alpha1"
)

var _ = Describe("ClaudeTask Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		claudetask := &claudetownv1alpha1.ClaudeTask{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind ClaudeTask")
			err := k8sClient.Get(ctx, typeNamespacedName, claudetask)
			if err != nil && errors.IsNotFound(err) {
				resource := &claudetownv1alpha1.ClaudeTask{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: claudetownv1alpha1.ClaudeTaskSpec{
						Repository: "test-org/test-repo",
						Issue:      1,
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &claudetownv1alpha1.ClaudeTask{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance ClaudeTask")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Setting status to Completed so reconcile is a no-op")
			resource := &claudetownv1alpha1.ClaudeTask{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			resource.Status.Phase = claudetownv1alpha1.ClaudeTaskPhaseCompleted
			Expect(k8sClient.Status().Update(ctx, resource)).To(Succeed())

			By("Reconciling the created resource")
			controllerReconciler := &ClaudeTaskReconciler{
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
