// SPDX-License-Identifier: Apache-2.0
// Copyright 2021-2022 Authors of Cilium

package k8sTest

import (
	"context"
	"fmt"
	"os"
	"time"

	. "github.com/onsi/gomega"

	. "github.com/cilium/cilium/test/ginkgo-ext"
	"github.com/cilium/cilium/test/helpers"
)

var _ = SkipDescribeIf(
	func() bool {
		// The 5.4 CI job is intended to catch BPF complexity regressions and
		// as such doesn't need to execute this test suite.
		return helpers.RunsOn54Kernel() ||
			// Test requests to the LB are going to be sent from the node which
			// doesn't run Cilium.
			helpers.DoesNotExistNodeWithoutCilium()
	}, "K8sBGPTests", func() {
		var (
			kubectl        *helpers.Kubectl
			ciliumFilename string
			ni             *nodesInfo
		)

		BeforeAll(func() {
			kubectl = helpers.CreateKubectl(helpers.K8s1VMName(), logger)
			ni = getNodesInfo(kubectl)
			ciliumFilename = helpers.TimestampFilename("cilium.yaml")
		})

		JustAfterEach(func() {
			kubectl.ValidateNoErrorsInLogs(CurrentGinkgoTestDescription().Duration)
		})

		AfterAll(func() {
			UninstallCiliumFromManifest(kubectl, ciliumFilename)
			kubectl.CloseSSHClient()
		})

		Context("Tests LoadBalancer", func() {
			var (
				frr      string // BGP router
				routerIP string

				bgpConfigMap string

				lbSVC string

				ciliumPodK8s1, ciliumPodK8s2 string
				testStartTime                time.Time
			)

			BeforeAll(func() {
				frr = applyFRRTemplate(kubectl, ni)
				kubectl.ApplyDefault(frr).ExpectSuccess("Unable to apply rendered template %s", frr)

				Eventually(func() string {
					frrPod, err := kubectl.GetPodsIPs(helpers.KubeSystemNamespace, "app=frr")
					if _, ok := frrPod["frr"]; err != nil || !ok {
						return ""
					}
					routerIP = frrPod["frr"]
					return routerIP
				}, 30*time.Second, 1*time.Second).Should(Not(BeEmpty()), "BGP router is not ready")

				bgpConfigMap = applyBGPCMTemplate(kubectl, routerIP)
				kubectl.ApplyDefault(bgpConfigMap).ExpectSuccess("Unable to apply BGP ConfigMap %s", bgpConfigMap)

				DeployCiliumOptionsAndDNS(kubectl, ciliumFilename,
					map[string]string{
						"bgp.enabled":                 "true",
						"bgp.announce.loadbalancerIP": "true",

						"debug.verbose":        "datapath", // https://github.com/cilium/cilium/issues/16399
						"tunnel":               "disabled",
						"autoDirectNodeRoutes": "true",
					})

				lbSVC = helpers.ManifestGet(kubectl.BasePath(), "test_lb_with_ip.yaml")
				kubectl.ApplyDefault(lbSVC).ExpectSuccess("Unable to apply %s", lbSVC)

				var err error
				ciliumPodK8s1, err = kubectl.GetCiliumPodOnNode(helpers.K8s1)
				ExpectWithOffset(1, err).ShouldNot(HaveOccurred(), "Cannot determine cilium pod name")
				ciliumPodK8s2, err = kubectl.GetCiliumPodOnNode(helpers.K8s2)
				ExpectWithOffset(1, err).ShouldNot(HaveOccurred(), "Cannot determine cilium pod name")
				testStartTime = time.Now()
			})

			AfterAll(func() {
				kubectl.Delete(frr)
				kubectl.Delete(bgpConfigMap)
				kubectl.Delete(lbSVC)
				// Delete temp files
				os.Remove(frr)
				os.Remove(bgpConfigMap)
				ExpectAllPodsTerminated(kubectl)
			})

			AfterFailed(func() {
				res := kubectl.CiliumExecContext(
					context.TODO(),
					ciliumPodK8s1,
					fmt.Sprintf(
						"hubble observe debug-events --since %v -o json",
						testStartTime.Format(time.RFC3339),
					),
				)
				helpers.WriteToReportFile(
					res.CombineOutput().Bytes(),
					"tests-loadbalancer-hubble-observe-debug-events-k8s1.log",
				)
				res = kubectl.CiliumExecContext(
					context.TODO(),
					ciliumPodK8s2,
					fmt.Sprintf(
						"hubble observe debug-events --since %v -o json",
						testStartTime.Format(time.RFC3339),
					),
				)
				helpers.WriteToReportFile(
					res.CombineOutput().Bytes(),
					"tests-loadbalancer-hubble-observe-debug-events-k8s2.log",
				)
			})

			It("Connectivity to endpoint via LB", func() {
				By("Waiting until the Operator has assigned the LB IP")
				lbIP, err := kubectl.GetLoadBalancerIP(
					helpers.DefaultNamespace, lbSvcName, 30*time.Second)
				Expect(err).Should(BeNil(), "Cannot retrieve LB IP for test-lb")

				By("Waiting until the Agents have announced the LB IP via BGP")
				Eventually(func() string {
					return kubectl.ExecInHostNetNS(
						context.TODO(),
						ni.outsideNodeName,
						"ip route",
					).GetStdOut().String()
				}, 30*time.Second, 1*time.Second).Should(ContainSubstring(lbIP),
					"BGP router does not have route for LB IP")

				// Check connectivity from outside
				url := "http://" + lbIP
				testCurlFromOutside(kubectl, ni, url, 10, false)

				// Patch service to add a LB source range to disallow requests
				// from the outsideNode
				kubectl.Patch(helpers.DefaultNamespace, "service", lbSvcName,
					`{"spec": {"loadBalancerSourceRanges": ["1.1.1.0/24"]}}`)
				time.Sleep(5 * time.Second)
				testCurlFailFromOutside(kubectl, ni, url, 1)
				// Patch again, but this time add outsideNode IP addr
				kubectl.Patch(helpers.DefaultNamespace, "service", lbSvcName,
					fmt.Sprintf(
						`{"spec": {"loadBalancerSourceRanges": ["1.1.1.0/24", "%s/32"]}}`,
						ni.outsideIP))
				time.Sleep(5 * time.Second)
				testCurlFromOutside(kubectl, ni, url, 10, false)
			})
		})
	})
