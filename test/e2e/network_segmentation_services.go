package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"

	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	kapi "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eservice "k8s.io/kubernetes/test/e2e/framework/service"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
)

var _ = Describe("Network Segmentation: services", func() {

	f := wrappedTestFramework("udn-services")

	Context("on a user defined primary network", func() {
		const (
			nadName                      = "tenant-red"
			servicePort                  = 80
			serviceTargetPort            = 80
			userDefinedNetworkIPv4Subnet = "10.128.0.0/16"
			userDefinedNetworkIPv6Subnet = "2014:100:200::0/60"
		)

		var (
			cs                  clientset.Interface
			nadClient           nadclient.K8sCniCncfIoV1Interface
			defaultNetNamespace string
		)

		BeforeEach(func() {
			cs = f.ClientSet

			var err error
			nadClient, err = nadclient.NewForConfig(f.ClientConfig())
			Expect(err).NotTo(HaveOccurred())
			defaultNetNamespace = ""
		})

		cleanupFn := func() {
			By("Removing the namespace so all resources get deleted")
			err := cs.CoreV1().Namespaces().Delete(context.TODO(), f.Namespace.Name, metav1.DeleteOptions{})
			framework.ExpectNoError(err, "Failed to remove the namespace %s %v", f.Namespace.Name, err)
			if defaultNetNamespace != "" {
				err = cs.CoreV1().Namespaces().Delete(context.TODO(), defaultNetNamespace, metav1.DeleteOptions{})
				framework.ExpectNoError(err, "Failed to remove the namespace %v", defaultNetNamespace, err)
			}

		}

		AfterEach(func() {
			cleanupFn()
		})

		DescribeTable(
			// The test creates a client and nodeport service in a UDN backed by one pod and similarly
			// a nodeport service and a client in the default network. We expect ClusterIPs to be
			// reachable only within the same network. We expect NodePort services to be exposed
			// to all networks.
			// We verify the following scenarios:
			// - UDN client --> UDN service, with backend pod and client running on the same node:
			//   + clusterIP succeeds
			//   + nodeIP:nodePort works, when we only target the local node
			//
			// - UDN client --> UDN service, with backend pod and client running on different nodes:
			//   + clusterIP succeeds
			//   + nodeIP:nodePort succeeds, when we only target the local node
			//
			// - default-network client --> UDN service:
			//   + clusterIP fails
			//   + nodeIP:nodePort fails FOR NOW, when we only target the local node
			//
			// -  UDN service --> default-network:
			//   + clusterIP fails
			//   + nodeIP:nodePort fails FOR NOW, when we only target the local node

			"should be reachable through their cluster IP and node port",
			func(
				netConfigParams networkAttachmentConfigParams,
			) {
				namespace := f.Namespace.Name
				jig := e2eservice.NewTestJig(cs, namespace, "udn-service")

				if netConfigParams.topology == "layer2" && !isInterconnectEnabled() {
					const upstreamIssue = "https://github.com/ovn-org/ovn-kubernetes/issues/4703"
					e2eskipper.Skipf(
						"Service e2e tests for layer2 topologies are known to fail on non-IC deployments. Upstream issue: %s", upstreamIssue,
					)
				}

				By("Selecting 3 schedulable nodes")
				nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
				Expect(err).NotTo(HaveOccurred())
				Expect(len(nodes.Items)).To(BeNumerically(">", 2))

				By("Selecting nodes for pods and service")
				serverPodNodeName := nodes.Items[0].Name
				clientNode := nodes.Items[1].Name // when client runs on a different node than the server

				By("Creating the attachment configuration")
				netConfig := newNetworkAttachmentConfig(netConfigParams)
				netConfig.namespace = f.Namespace.Name
				_, err = nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Create(
					context.Background(),
					generateNAD(netConfig),
					metav1.CreateOptions{},
				)
				Expect(err).NotTo(HaveOccurred())

				By(fmt.Sprintf("Creating a UDN NodePort service"))
				policy := v1.IPFamilyPolicyPreferDualStack
				udnService, err := jig.CreateUDPService(context.TODO(), func(s *v1.Service) {
					s.Spec.Ports = []v1.ServicePort{
						{
							Name:       "udp",
							Protocol:   v1.ProtocolUDP,
							Port:       80,
							TargetPort: intstr.FromInt(int(serviceTargetPort)),
						},
					}
					s.Spec.Type = v1.ServiceTypeNodePort
					s.Spec.IPFamilyPolicy = &policy
				})
				framework.ExpectNoError(err)

				By("Creating a UDN backend pod")
				udnServerPod := e2epod.NewAgnhostPod(
					namespace, "backend-pod", nil, nil,
					[]v1.ContainerPort{
						{ContainerPort: (serviceTargetPort), Protocol: "UDP"}},
					"netexec",
					"--udp-port="+fmt.Sprint(serviceTargetPort))

				udnServerPod.Labels = jig.Labels
				udnServerPod.Spec.NodeName = serverPodNodeName
				udnServerPod = e2epod.NewPodClient(f).CreateSync(context.TODO(), udnServerPod)

				By(fmt.Sprintf("Creating a UDN client pod on the same node (%s)", udnServerPod.Spec.NodeName))
				udnClientPod := e2epod.NewAgnhostPod(namespace, "udn-client", nil, nil, nil)
				udnClientPod.Spec.NodeName = udnServerPod.Spec.NodeName
				udnClientPod = e2epod.NewPodClient(f).CreateSync(context.TODO(), udnClientPod)

				// UDN -> UDN
				By("Connect to the UDN service cluster IP from the UDN client pod on the same node")
				checkConnectionToClusterIPs(f, udnClientPod, udnService, udnServerPod.Name)
				By("Connect to the UDN service nodePort on all 3 nodes from the UDN client pod")
				checkConnectionToNodePort(f, udnClientPod, udnService, &nodes.Items[0], "endpoint node", udnServerPod.Name)
				// FIXME(dceara): Remove this check when Local Gateway external->service support is implemented.
				if !IsGatewayModeLocal() {
					checkConnectionToNodePort(f, udnClientPod, udnService, &nodes.Items[1], "other node", udnServerPod.Name)
					checkConnectionToNodePort(f, udnClientPod, udnService, &nodes.Items[2], "other node", udnServerPod.Name)
				}

				By(fmt.Sprintf("Creating a UDN client pod on a different node (%s)", clientNode))
				udnClientPod2 := e2epod.NewAgnhostPod(namespace, "udn-client2", nil, nil, nil)
				udnClientPod2.Spec.NodeName = clientNode
				udnClientPod2 = e2epod.NewPodClient(f).CreateSync(context.TODO(), udnClientPod2)

				By("Connect to the UDN service from the UDN client pod on a different node")
				checkConnectionToClusterIPs(f, udnClientPod2, udnService, udnServerPod.Name)
				checkConnectionToNodePort(f, udnClientPod2, udnService, &nodes.Items[1], "local node", udnServerPod.Name)
				// FIXME(dceara): Remove this check when Local Gateway external->service support is implemented.
				if !IsGatewayModeLocal() {
					checkConnectionToNodePort(f, udnClientPod2, udnService, &nodes.Items[0], "server node", udnServerPod.Name)
					checkConnectionToNodePort(f, udnClientPod2, udnService, &nodes.Items[2], "other node", udnServerPod.Name)
				}

				// Default network -> UDN
				// Check that it cannot connect
				By(fmt.Sprintf("Create a client pod in the default network on node %s", clientNode))
				defaultNetNamespace = f.Namespace.Name + "-default"
				_, err = cs.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: defaultNetNamespace,
					},
				}, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				defaultClient, err := createPod(f, "default-net-pod", clientNode, defaultNetNamespace, []string{"sleep", "2000000"}, nil)
				Expect(err).NotTo(HaveOccurred())

				By("Verify the connection of the client in the default network to the UDN service")
				checkNoConnectionToClusterIPs(f, defaultClient, udnService)

				checkNoConnectionToNodePort(f, defaultClient, udnService, &nodes.Items[1], "local node") // TODO change to checkConnectionToNodePort when we have full UDN support in ovnkube-node

				// FIXME(dceara): Remove this check when Local Gateway external->service support is implemented.
				if !IsGatewayModeLocal() {
					checkConnectionToNodePort(f, defaultClient, udnService, &nodes.Items[0], "server node", udnServerPod.Name)
					checkConnectionToNodePort(f, defaultClient, udnService, &nodes.Items[2], "other node", udnServerPod.Name)
				}

				// UDN -> Default network
				// Create a backend pod and service in the default network and verify that the client pod in the UDN
				// cannot reach it
				By(fmt.Sprintf("Creating a backend pod in the default network on node %s", serverPodNodeName))
				defaultLabels := map[string]string{"app": "default-app"}

				defaultServerPod, err := createPod(f, "backend-pod-default", serverPodNodeName,
					defaultNetNamespace, []string{"/agnhost", "netexec", "--udp-port=" + fmt.Sprint(serviceTargetPort)}, defaultLabels,
					func(pod *v1.Pod) {
						pod.Spec.Containers[0].Ports = []v1.ContainerPort{{ContainerPort: (serviceTargetPort), Protocol: "UDP"}}
					})
				Expect(err).NotTo(HaveOccurred())

				By("create a node port service in the default network")
				defaultService := &v1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: "service-default"},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{
							{
								Name:     "udp-port",
								Port:     int32(servicePort),
								Protocol: v1.ProtocolUDP,
							},
						},
						Selector:       defaultLabels,
						Type:           v1.ServiceTypeNodePort,
						IPFamilyPolicy: &policy,
					},
				}

				defaultService, err = f.ClientSet.CoreV1().Services(defaultNetNamespace).Create(context.TODO(), defaultService, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				By("Verify the UDN client connection to the default network service")
				checkConnectionToNodePort(f, udnClientPod2, defaultService, &nodes.Items[0], "server node", defaultServerPod.Name)
				checkNoConnectionToNodePort(f, udnClientPod2, defaultService, &nodes.Items[1], "local node")
				checkConnectionToNodePort(f, udnClientPod2, defaultService, &nodes.Items[2], "other node", defaultServerPod.Name)
				checkNoConnectionToClusterIPs(f, udnClientPod2, defaultService)

				// Make sure that restarting OVNK after applying a UDN with an affected service won't result
				// in OVNK in CLBO state https://issues.redhat.com/browse/OCPBUGS-41499
				if netConfigParams.topology == "layer3" { // no need to run it for layer 2 as well
					By("Restart ovnkube-node on one node and verify that the new ovnkube-node pod goes to the running state")
					err = restartOVNKubeNodePod(cs, ovnNamespace, clientNode)
					Expect(err).NotTo(HaveOccurred())
				}
			},

			Entry(
				"L3 primary UDN, cluster-networked pods, NodePort service",
				networkAttachmentConfigParams{
					name:     nadName,
					topology: "layer3",
					cidr:     correctCIDRFamily(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
					role:     "primary",
				},
			),
			Entry(
				"L2 primary UDN, cluster-networked pods, NodePort service",
				networkAttachmentConfigParams{
					name:     nadName,
					topology: "layer2",
					cidr:     correctCIDRFamily(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
					role:     "primary",
				},
			),
		)

	})

})

// TODO Once https://github.com/ovn-org/ovn-kubernetes/pull/4567 merges, use the vendored *TestJig.Run(), which tests
// the reachability of a service through its name and through its cluster IP. For now only test the cluster IP.

const OvnNodeIfAddr = "k8s.ovn.org/node-primary-ifaddr"

type primaryIfAddrAnnotation struct {
	IPv4 string `json:"ipv4,omitempty"`
	IPv6 string `json:"ipv6,omitempty"`
}

// ParseNodeHostIPDropNetMask returns the parsed host IP addresses found on a node's host CIDR annotation. Removes the mask.
func ParseNodeHostIPDropNetMask(node *kapi.Node) (sets.Set[string], error) {
	nodeIfAddrAnnotation, ok := node.Annotations[OvnNodeIfAddr]
	if !ok {
		return nil, newAnnotationNotSetError("%s annotation not found for node %q", OvnNodeIfAddr, node.Name)
	}
	nodeIfAddr := &primaryIfAddrAnnotation{}
	if err := json.Unmarshal([]byte(nodeIfAddrAnnotation), nodeIfAddr); err != nil {
		return nil, fmt.Errorf("failed to unmarshal annotation: %s for node %q, err: %v", OvnNodeIfAddr, node.Name, err)
	}

	var cfg []string
	if nodeIfAddr.IPv4 != "" {
		cfg = append(cfg, nodeIfAddr.IPv4)
	}
	if nodeIfAddr.IPv6 != "" {
		cfg = append(cfg, nodeIfAddr.IPv6)
	}
	if len(cfg) == 0 {
		return nil, fmt.Errorf("node: %q does not have any IP information set", node.Name)
	}

	for i, cidr := range cfg {
		ip, _, err := net.ParseCIDR(cidr)
		if err != nil || ip == nil {
			return nil, fmt.Errorf("failed to parse node host cidr: %v", err)
		}
		cfg[i] = ip.String()
	}
	return sets.New(cfg...), nil
}

func checkConnectionToAgnhostPod(f *framework.Framework, clientPod *v1.Pod, expectedOutput, cmd string) error {
	return wait.PollImmediate(200*time.Millisecond, 5*time.Second, func() (bool, error) {
		stdout, stderr, err2 := ExecShellInPodWithFullOutput(f, clientPod.Namespace, clientPod.Name, cmd)
		fmt.Printf("stdout=%s\n", stdout)
		fmt.Printf("stderr=%s\n", stderr)
		fmt.Printf("err=%v\n", err2)

		if stderr != "" {
			return false, fmt.Errorf("stderr=%s", stderr)
		}

		if err2 != nil {
			return false, err2
		}
		return stdout == expectedOutput, nil
	})
}

func checkNoConnectionToAgnhostPod(f *framework.Framework, clientPod *v1.Pod, cmd string) error {
	err := wait.PollImmediate(500*time.Millisecond, 2*time.Second, func() (bool, error) {
		stdout, stderr, err2 := ExecShellInPodWithFullOutput(f, clientPod.Namespace, clientPod.Name, cmd)
		fmt.Printf("stdout=%s\n", stdout)
		fmt.Printf("stderr=%s\n", stderr)
		fmt.Printf("err=%v\n", err2)

		if stderr != "" {
			return false, nil
		}

		if err2 != nil {
			return false, err2
		}
		if stdout != "" {
			return true, fmt.Errorf("Connection unexpectedly succeeded. Stdout: %s\n", stdout)
		}
		return false, nil
	})

	if err != nil {
		if wait.Interrupted(err) {
			// The timeout occurred without the connection succeeding, which is what we expect
			return nil
		}
		return err
	}
	return fmt.Errorf("Error: %s/%s was able to connect (cmd=%s) ", clientPod.Namespace, clientPod.Name, cmd)
}

func checkConnectionToClusterIPs(f *framework.Framework, clientPod *v1.Pod, service *v1.Service, expectedOutput string) {
	checkConnectionOrNoConnectionToClusterIPs(f, clientPod, service, expectedOutput, true)
}

func checkNoConnectionToClusterIPs(f *framework.Framework, clientPod *v1.Pod, service *v1.Service) {
	checkConnectionOrNoConnectionToClusterIPs(f, clientPod, service, "", false)
}

func checkConnectionOrNoConnectionToClusterIPs(f *framework.Framework, clientPod *v1.Pod, service *v1.Service, expectedOutput string, shouldConnect bool) {
	var err error
	targetPort := service.Spec.Ports[0].TargetPort.String()
	notStr := ""
	if !shouldConnect {
		notStr = "not "
	}

	for _, clusterIP := range service.Spec.ClusterIPs {
		msg := fmt.Sprintf("Client %s/%s should %sreach service %s/%s on cluster IP %s port %s",
			clientPod.Namespace, clientPod.Name, notStr, service.Namespace, service.Name, clusterIP, targetPort)
		By(msg)

		cmd := fmt.Sprintf(`/bin/sh -c 'echo hostname | nc -u -w 1 %s %s '`, clusterIP, targetPort)

		if shouldConnect {
			err = checkConnectionToAgnhostPod(f, clientPod, expectedOutput, cmd)
		} else {
			err = checkNoConnectionToAgnhostPod(f, clientPod, cmd)
		}
		framework.ExpectNoError(err, fmt.Sprintf("Failed to verify that %s", msg))
	}
}

func checkConnectionToNodePort(f *framework.Framework, clientPod *v1.Pod, service *v1.Service, node *v1.Node, nodeRoleMsg, expectedOutput string) {
	checkConnectionOrNoConnectionToNodePort(f, clientPod, service, node, nodeRoleMsg, expectedOutput, true)
}

func checkNoConnectionToNodePort(f *framework.Framework, clientPod *v1.Pod, service *v1.Service, node *v1.Node, nodeRoleMsg string) {
	checkConnectionOrNoConnectionToNodePort(f, clientPod, service, node, nodeRoleMsg, "", false)
}

func checkConnectionOrNoConnectionToNodePort(f *framework.Framework, clientPod *v1.Pod, service *v1.Service, node *v1.Node, nodeRoleMsg, expectedOutput string, shouldConnect bool) {
	var err error
	nodePort := service.Spec.Ports[0].NodePort
	notStr := ""
	if !shouldConnect {
		notStr = "not "
	}
	nodeIPs, err := ParseNodeHostIPDropNetMask(node)
	Expect(err).NotTo(HaveOccurred())

	for nodeIP := range nodeIPs {
		msg := fmt.Sprintf("Client %s/%s should %sconnect to NodePort service %s/%s on %s:%d (node %s, %s)",
			clientPod.Namespace, clientPod.Name, notStr, service.Namespace, service.Name, nodeIP, nodePort, node.Name, nodeRoleMsg)
		By(msg)
		cmd := fmt.Sprintf(`/bin/sh -c 'echo hostname | nc -u -w 1 %s %d '`, nodeIP, nodePort)

		if shouldConnect {
			err = checkConnectionToAgnhostPod(f, clientPod, expectedOutput, cmd)
		} else {
			err = checkNoConnectionToAgnhostPod(f, clientPod, cmd)
		}
		framework.ExpectNoError(err, fmt.Sprintf("Failed to verify that %s", msg))
	}
}