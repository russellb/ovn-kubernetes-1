package ovn

import (
	"fmt"

	"github.com/urfave/cli"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func newPodMeta(namespace, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      name,
		UID:       types.UID(name),
		Namespace: namespace,
		Labels: map[string]string{
			"name": name,
		},
	}
}

func newPod(namespace, name, node, podIP string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: newPodMeta(namespace, name),
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "containerName",
					Image: "containerImage",
				},
			},
			NodeName: node,
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
			PodIP: podIP,
		},
	}
}

type pod struct {
	nodeName   string
	nodeSubnet string
	nodeMgtIP  string
	nodeGWIP   string
	podName    string
	podIP      string
	podMAC     string
	namespace  string
	portName   string
}

func newTPod(nodeName, nodeSubnet, nodeMgtIP, nodeGWIP, podName, podIP, podMAC, namespace string) (to pod) {
	to = pod{
		nodeName:   nodeName,
		nodeSubnet: nodeSubnet,
		nodeMgtIP:  nodeMgtIP,
		nodeGWIP:   nodeGWIP,
		podName:    podName,
		podIP:      podIP,
		podMAC:     podMAC,
		namespace:  namespace,
		portName:   namespace + "_" + podName,
	}
	return
}

func (p pod) addCmds(fexec *ovntest.FakeExec, exists bool) {
	// node setup
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 get logical_switch " + p.nodeName + " other-config",
		Output: `{exclude_ips="10.128.1.2", subnet="` + p.nodeSubnet + `"}`,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 get logical_switch " + p.nodeName + " other-config:subnet",
		Output: fmt.Sprintf("%q", p.nodeSubnet),
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --may-exist acl-add " + p.nodeName + " to-lport 1001 ip4.src==" + p.nodeMgtIP + " allow-related",
	})
	// pod setup
	if exists {
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 --may-exist lsp-add %s %s -- lsp-set-addresses %s %s %s -- set logical_switch_port %s external-ids:namespace=namespace external-ids:logical_switch=%s external-ids:pod=true -- --if-exists clear logical_switch_port %s dynamic_addresses", p.nodeName, p.portName, p.portName, p.podMAC, p.podIP, p.portName, p.nodeName, p.portName),
		})
	} else {
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovn-nbctl --timeout=15 --wait=sb -- --may-exist lsp-add " + p.nodeName + " " + p.portName + " -- lsp-set-addresses " + p.portName + " dynamic -- set logical_switch_port " + p.portName + " external-ids:namespace=" + p.namespace + " external-ids:logical_switch=" + p.nodeName + " external-ids:pod=true",
		})
	}
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --if-exists get logical_switch " + p.nodeName + " external_ids:gateway_ip",
		Output: fmt.Sprintf("%s/24", p.nodeGWIP),
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 get logical_switch_port " + p.portName + " dynamic_addresses",
		Output: `"` + p.podMAC + " " + p.podIP + `"`,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 lsp-set-port-security " + p.portName + " " + p.podMAC + " " + p.podIP + "/24",
	})
}

func (p pod) addCmdsForNonExistingPod(fexec *ovntest.FakeExec) {
	p.addCmds(fexec, false)
}

func (p pod) addCmdsForExistingPod(fexec *ovntest.FakeExec) {
	p.addCmds(fexec, true)
}

func (p pod) delCmds(fexec *ovntest.FakeExec) {
	// pod's logical switch port is removed
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --if-exists lsp-del " + p.portName,
	})
}

var _ = Describe("OVN Pod Operations", func() {
	var app *cli.App

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.RestoreDefaultConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
	})

	Context("during execution", func() {

		It("reconciles an existing pod", func() {
			app.Action = func(ctx *cli.Context) error {

				// Setup an unassigned pod, perform an update later on which assigns it.
				t := newTPod(
					"",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.4",
					"11:22:33:44:55:66",
					"namespace",
				)

				fExec := ovntest.NewFakeExec()
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: "\n",
				})

				fakeOvn := FakeOVN{}
				fakeOvn.start(ctx, fExec, &v1.PodList{
					Items: []v1.Pod{
						*newPod(t.namespace, t.podName, t.nodeName, t.podIP),
					},
				})
				fakeOvn.controller.WatchPods()

				pod, err := fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Get(t.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(fExec.CalledMatchesExpected()).To(BeTrue())

				_, ok := pod.Annotations["ovn"]
				Expect(ok).To(BeFalse())

				// Assign it and perform the update
				t.nodeName = "node1"
				t.addCmdsForNonExistingPod(fExec)

				_, err = fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Update(newPod(t.namespace, t.podName, t.nodeName, t.podIP))
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue())

				pod, err = fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Get(t.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())

				podAnnotation, ok := pod.Annotations["ovn"]
				Expect(ok).To(BeTrue())
				Expect(podAnnotation).To(MatchJSON(`{"ip_address":"` + t.podIP + `/24", "mac_address":"` + t.podMAC + `", "gateway_ip": "` + t.nodeGWIP + `"}`))

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles a new pod", func() {
			app.Action = func(ctx *cli.Context) error {

				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.4",
					"11:22:33:44:55:66",
					"namespace",
				)

				fExec := ovntest.NewFakeExec()
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: "\n",
				})

				fakeOvn := FakeOVN{}
				fakeOvn.start(ctx, fExec, &v1.PodList{
					Items: []v1.Pod{},
				})
				fakeOvn.controller.WatchPods()

				pod, _ := fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Get(t.podName, metav1.GetOptions{})
				Expect(pod).To(BeNil())
				Expect(fExec.CalledMatchesExpected()).To(BeTrue())

				t.addCmdsForNonExistingPod(fExec)

				_, err := fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Create(newPod(t.namespace, t.podName, t.nodeName, t.podIP))
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue())

				pod, err = fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Get(t.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())

				podAnnotation, ok := pod.Annotations["ovn"]
				Expect(ok).To(BeTrue())
				Expect(podAnnotation).To(MatchJSON(`{"ip_address":"` + t.podIP + `/24", "mac_address":"` + t.podMAC + `", "gateway_ip": "` + t.nodeGWIP + `"}`))

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles a deleted pod", func() {
			app.Action = func(ctx *cli.Context) error {

				// Setup an assigned pod
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.4",
					"11:22:33:44:55:66",
					"namespace",
				)

				fExec := ovntest.NewFakeExec()
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: t.portName + "\n",
				})

				t.addCmdsForNonExistingPod(fExec)

				fakeOvn := FakeOVN{}
				fakeOvn.start(ctx, fExec, &v1.PodList{
					Items: []v1.Pod{
						*newPod(t.namespace, t.podName, t.nodeName, t.podIP),
					},
				})
				fakeOvn.controller.WatchPods()

				pod, err := fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Get(t.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())

				podAnnotation, ok := pod.Annotations["ovn"]
				Expect(ok).To(BeTrue())
				Expect(podAnnotation).To(MatchJSON(`{"ip_address":"` + t.podIP + `/24", "mac_address":"` + t.podMAC + `", "gateway_ip": "` + t.nodeGWIP + `"}`))
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue())

				// Delete it
				t.delCmds(fExec)

				err = fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Delete(t.podName, metav1.NewDeleteOptions(0))
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue())

				pod, err = fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Get(t.podName, metav1.GetOptions{})
				Expect(err).To(HaveOccurred())
				Expect(pod).To(BeNil())

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

	})

	Context("on startup", func() {

		It("reconciles an existing pod", func() {
			app.Action = func(ctx *cli.Context) error {

				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.4",
					"11:22:33:44:55:66",
					"namespace",
				)

				fExec := ovntest.NewFakeExec()
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: t.portName + "\n",
				})

				t.addCmdsForNonExistingPod(fExec)

				fakeOvn := FakeOVN{}
				fakeOvn.start(ctx, fExec, &v1.PodList{
					Items: []v1.Pod{
						*newPod(t.namespace, t.podName, t.nodeName, t.podIP),
					},
				})

				pod, err := fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Get(t.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(fExec.CalledMatchesExpected()).To(BeZero())

				_, ok := pod.Annotations["ovn"]
				Expect(ok).To(BeFalse())

				fakeOvn.controller.WatchPods()

				pod, err = fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Get(t.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(fExec.CalledMatchesExpected()).To(BeTrue())

				podAnnotation, ok := pod.Annotations["ovn"]
				Expect(ok).To(BeTrue())
				Expect(podAnnotation).To(MatchJSON(`{"ip_address":"` + t.podIP + `/24", "mac_address":"` + t.podMAC + `", "gateway_ip": "` + t.nodeGWIP + `"}`))

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles a deleted pod", func() {
			app.Action = func(ctx *cli.Context) error {

				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.4",
					"11:22:33:44:55:66",
					"namespace",
				)

				fExec := ovntest.NewFakeExec()
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: t.portName + "\n",
				})

				t.delCmds(fExec)

				fakeOvn := FakeOVN{}
				fakeOvn.start(ctx, fExec)
				fakeOvn.controller.WatchPods()

				Expect(fExec.CalledMatchesExpected()).To(BeTrue())

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles a new pod", func() {
			app.Action = func(ctx *cli.Context) error {

				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.4",
					"11:22:33:44:55:66",
					"namespace",
				)

				fExec := ovntest.NewFakeExec()
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: "\n",
				})

				t.addCmdsForNonExistingPod(fExec)

				fakeOvn := FakeOVN{}
				fakeOvn.start(ctx, fExec, &v1.PodList{
					Items: []v1.Pod{
						*newPod(t.namespace, t.podName, t.nodeName, t.podIP),
					},
				})
				fakeOvn.controller.WatchPods()

				pod, err := fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Get(t.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(fExec.CalledMatchesExpected()).To(BeTrue())

				podAnnotation, ok := pod.Annotations["ovn"]
				Expect(ok).To(BeTrue())
				Expect(podAnnotation).To(MatchJSON(`{"ip_address":"` + t.podIP + `/24", "mac_address":"` + t.podMAC + `", "gateway_ip": "` + t.nodeGWIP + `"}`))

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("on ovn restart", func() {

		It("reconciles an existing pod", func() {
			app.Action = func(ctx *cli.Context) error {

				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.4",
					"11:22:33:44:55:66",
					"namespace",
				)

				fExec := ovntest.NewFakeExec()
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: "\n",
				})

				t.addCmdsForNonExistingPod(fExec)

				fakeOvn := FakeOVN{}
				fakeOvn.start(ctx, fExec, &v1.PodList{
					Items: []v1.Pod{
						*newPod(t.namespace, t.podName, t.nodeName, t.podIP),
					},
				})
				fakeOvn.controller.WatchPods()

				pod, err := fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Get(t.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(fExec.CalledMatchesExpected()).To(BeTrue())

				podAnnotation, ok := pod.Annotations["ovn"]
				Expect(ok).To(BeTrue())
				Expect(podAnnotation).To(MatchJSON(`{"ip_address":"` + t.podIP + `/24", "mac_address":"` + t.podMAC + `", "gateway_ip": "` + t.nodeGWIP + `"}`))

				// Simulate an OVN restart with a new IP assignment and verify that the pod annotation is updated.
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: "\n",
				})

				t.addCmdsForExistingPod(fExec)

				fakeOvn.restart()
				fakeOvn.controller.WatchPods()

				pod, err = fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Get(t.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue())

				// Check that pod annotations have been re-written to correct values
				podAnnotation, ok = pod.Annotations["ovn"]
				Expect(ok).To(BeTrue())
				Expect(podAnnotation).To(MatchJSON(`{"ip_address":"` + t.podIP + `/24", "mac_address":"` + t.podMAC + `", "gateway_ip": "` + t.nodeGWIP + `"}`))

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
