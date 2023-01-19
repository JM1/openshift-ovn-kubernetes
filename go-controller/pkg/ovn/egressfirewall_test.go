package ovn

import (
	"context"
	"fmt"
	"net"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	"github.com/urfave/cli/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	t "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func newObjectMeta(name, namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		UID:       types.UID(namespace),
		Name:      name,
		Namespace: namespace,
	}

}

func newEgressFirewallObject(name, namespace string, egressRules []egressfirewallapi.EgressFirewallRule) *egressfirewallapi.EgressFirewall {

	return &egressfirewallapi.EgressFirewall{
		ObjectMeta: newObjectMeta(name, namespace),
		Spec: egressfirewallapi.EgressFirewallSpec{
			Egress: egressRules,
		},
	}
}

var _ = ginkgo.Describe("OVN EgressFirewall Operations", func() {
	var (
		app                    *cli.App
		fakeOVN                *FakeOVN
		clusterPortGroup       *nbdb.PortGroup
		nodeSwitch, joinSwitch *nbdb.LogicalSwitch
		initialData            []libovsdbtest.TestData
		dbSetup                libovsdbtest.TestSetup
	)
	const (
		node1Name string = "node1"
		node2Name string = "node2"
	)

	clusterRouter := &nbdb.LogicalRouter{
		UUID: t.OVNClusterRouter + "-UUID",
		Name: t.OVNClusterRouter,
	}

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()
		config.OVNKubernetesFeature.EnableEgressFirewall = true

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOVN = NewFakeOVN()
		clusterPortGroup = &nbdb.PortGroup{
			UUID: t.ClusterPortGroupName + "-UUID",
			Name: t.ClusterPortGroupName,
			ExternalIDs: map[string]string{
				"name": t.ClusterPortGroupName,
			},
		}
		nodeSwitch = &nbdb.LogicalSwitch{
			UUID: node1Name + "-UUID",
			Name: node1Name,
		}
		joinSwitch = &nbdb.LogicalSwitch{
			UUID: "join-UUID",
			Name: "join",
		}
		initialData = []libovsdbtest.TestData{
			nodeSwitch,
			joinSwitch,
			clusterPortGroup,
			clusterRouter,
		}
		dbSetup = libovsdbtest.TestSetup{
			NBData: initialData,
		}
	})

	ginkgo.AfterEach(func() {
		fakeOVN.shutdown()
	})

	ginkgo.Context("on startup", func() {
		for _, gwMode := range []config.GatewayMode{config.GatewayModeLocal, config.GatewayModeShared} {
			config.Gateway.Mode = gwMode
			ginkgo.It(fmt.Sprintf("reconciles stale ACLs, gateway mode %s", gwMode), func() {
				app.Action = func(ctx *cli.Context) error {

					purgeACL := libovsdbops.BuildACL(
						"",
						t.DirectionFromLPort,
						t.EgressFirewallStartPriority,
						"",
						nbdb.ACLActionDrop,
						"",
						"",
						false,
						map[string]string{egressFirewallACLExtIdKey: "none"},
					)
					purgeACL.UUID = libovsdbops.BuildNamedUUID()

					namespace1 := *newNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})
					keepACL := libovsdbops.BuildACL(
						"",
						t.DirectionFromLPort,
						t.EgressFirewallStartPriority,
						"(ip4.dst == 1.2.3.4/23) && ip4.src == $a10481622940199974102 && ip4.dst != 10.128.0.0/14",
						nbdb.ACLActionAllow,
						"",
						"",
						false,
						map[string]string{egressFirewallACLExtIdKey: namespace1.Name},
					)
					keepACL.UUID = libovsdbops.BuildNamedUUID()

					// this ACL is not in the egress firewall priority range and should be untouched
					otherACL := libovsdbops.BuildACL(
						"",
						t.DirectionFromLPort,
						t.MinimumReservedEgressFirewallPriority-1,
						"",
						nbdb.ACLActionDrop,
						"",
						"",
						false,
						map[string]string{egressFirewallACLExtIdKey: "default"},
					)
					otherACL.UUID = libovsdbops.BuildNamedUUID()

					nodeSwitch.ACLs = []string{purgeACL.UUID, keepACL.UUID}
					joinSwitch.ACLs = []string{purgeACL.UUID, keepACL.UUID}

					dbSetup := libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							otherACL,
							purgeACL,
							keepACL,
							nodeSwitch,
							joinSwitch,
							clusterRouter,
							clusterPortGroup,
						},
					}

					fakeOVN.startWithDBSetup(dbSetup,
						&v1.NodeList{
							Items: []v1.Node{
								{
									Status: v1.NodeStatus{
										Phase: v1.NodeRunning,
									},
									ObjectMeta: newObjectMeta(node1Name, ""),
								},
							},
						})

					// only create one egressFirewall
					_, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(namespace1.Name).
						Create(context.TODO(), egressFirewall, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOVN.controller.WatchEgressFirewall()

					// Both ACLs will be removed from the join switch
					joinSwitch.ACLs = nil
					// Both ACLs will be removed from the node switch
					nodeSwitch.ACLs = nil

					// keepACL will be added to the clusterPortGroup
					clusterPortGroup.ACLs = []string{keepACL.UUID}

					// Direction of both ACLs will be converted to
					keepACL.Direction = t.DirectionToLPort

					// purgeACL ACL will be deleted when test server starts deleting dereferenced ACLs
					// for now we need to update its fields, since it is present in the db
					purgeACL.Direction = t.DirectionToLPort

					expectedDatabaseState := []libovsdb.TestData{
						otherACL,
						purgeACL,
						keepACL,
						nodeSwitch,
						joinSwitch,
						clusterRouter,
						clusterPortGroup,
					}

					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

			})
			ginkgo.It(fmt.Sprintf("reconciles an existing egressFirewall with IPv4 CIDR, gateway mode %s", gwMode), func() {
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})

					fakeOVN.startWithDBSetup(dbSetup,
						&egressfirewallapi.EgressFirewallList{
							Items: []egressfirewallapi.EgressFirewall{
								*egressFirewall,
							},
						},
						&v1.NodeList{
							Items: []v1.Node{
								{
									Status: v1.NodeStatus{
										Phase: v1.NodeRunning,
									},
									ObjectMeta: newObjectMeta(node1Name, ""),
								},
							},
						})

					fakeOVN.controller.WatchEgressFirewall()

					_, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					ipv4ACL := libovsdbops.BuildACL(
						"",
						t.DirectionToLPort,
						t.EgressFirewallStartPriority,
						"(ip4.dst == 1.2.3.4/23) && ip4.src == $a10481622940199974102 && ip4.dst != 10.128.0.0/14",
						nbdb.ACLActionAllow,
						"",
						"",
						false,
						map[string]string{egressFirewallACLExtIdKey: "namespace1"},
					)
					ipv4ACL.UUID = libovsdbops.BuildNamedUUID()

					// new ACL will be added to the port group
					clusterPortGroup.ACLs = []string{ipv4ACL.UUID}
					expectedDatabaseState := append(initialData, ipv4ACL)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

			})
			ginkgo.It(fmt.Sprintf("reconciles an existing egressFirewall with IPv6 CIDR, gateway mode %s", gwMode), func() {
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "2002::1234:abcd:ffff:c0a8:101/64",
							},
						},
					})

					fakeOVN.startWithDBSetup(dbSetup,
						&egressfirewallapi.EgressFirewallList{
							Items: []egressfirewallapi.EgressFirewall{
								*egressFirewall,
							},
						},
						&v1.NamespaceList{
							Items: []v1.Namespace{
								namespace1,
							},
						}, &v1.NodeList{
							Items: []v1.Node{
								{
									Status: v1.NodeStatus{
										Phase: v1.NodeRunning,
									},
									ObjectMeta: newObjectMeta(node1Name, ""),
								},
							},
						})
					config.IPv6Mode = true
					fakeOVN.controller.WatchNamespaces()
					fakeOVN.controller.WatchEgressFirewall()

					_, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					ipv6ACL := libovsdbops.BuildACL(
						"",
						t.DirectionToLPort,
						t.EgressFirewallStartPriority,
						"(ip6.dst == 2002::1234:abcd:ffff:c0a8:101/64) && (ip4.src == $a10481622940199974102 || ip6.src == $a10481620741176717680) && ip4.dst != 10.128.0.0/14",
						nbdb.ACLActionAllow,
						"",
						"",
						false,
						map[string]string{egressFirewallACLExtIdKey: "namespace1"},
					)
					ipv6ACL.UUID = libovsdbops.BuildNamedUUID()

					// new ACL will be added to the port group
					clusterPortGroup.ACLs = []string{ipv6ACL.UUID}
					expectedDatabaseState := append(initialData, ipv6ACL)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

			})
		}
	})
	ginkgo.Context("during execution", func() {
		for _, gwMode := range []config.GatewayMode{config.GatewayModeLocal, config.GatewayModeShared} {
			config.Gateway.Mode = gwMode
			ginkgo.It(fmt.Sprintf("correctly creates an egressfirewall denying traffic udp traffic on port 100, gateway mode %s", gwMode), func() {
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Deny",
							Ports: []egressfirewallapi.EgressFirewallPort{
								{
									Protocol: "UDP",
									Port:     100,
								},
							},
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})
					fakeOVN.startWithDBSetup(dbSetup,
						&egressfirewallapi.EgressFirewallList{
							Items: []egressfirewallapi.EgressFirewall{
								*egressFirewall,
							},
						},
						&v1.NamespaceList{
							Items: []v1.Namespace{
								namespace1,
							},
						},
						&v1.NodeList{
							Items: []v1.Node{
								{
									Status: v1.NodeStatus{
										Phase: v1.NodeRunning,
									},
									ObjectMeta: newObjectMeta(node1Name, ""),
								},
							},
						})

					fakeOVN.controller.WatchNamespaces()
					_, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					fakeOVN.controller.WatchEgressFirewall()

					udpACL := libovsdbops.BuildACL(
						"",
						t.DirectionToLPort,
						t.EgressFirewallStartPriority,
						"(ip4.dst == 1.2.3.4/23) && ip4.src == $a10481622940199974102 && ((udp && ( udp.dst == 100 ))) && ip4.dst != 10.128.0.0/14",
						nbdb.ACLActionDrop,
						"",
						"",
						false,
						map[string]string{egressFirewallACLExtIdKey: "namespace1"},
					)

					udpACL.UUID = libovsdbops.BuildNamedUUID()

					// new ACL will be added to the port group
					clusterPortGroup.ACLs = []string{udpACL.UUID}
					expectedDatabaseState := append(initialData, udpACL)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}
				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
			ginkgo.It(fmt.Sprintf("correctly deletes an egressfirewall, gateway mode %s", gwMode), func() {
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							Ports: []egressfirewallapi.EgressFirewallPort{
								{
									Protocol: "TCP",
									Port:     100,
								},
							},
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.5/23",
							},
						},
					})

					fakeOVN.startWithDBSetup(dbSetup,
						&egressfirewallapi.EgressFirewallList{
							Items: []egressfirewallapi.EgressFirewall{
								*egressFirewall,
							},
						},
						&v1.NodeList{
							Items: []v1.Node{
								{
									Status: v1.NodeStatus{
										Phase: v1.NodeRunning,
									},
									ObjectMeta: newObjectMeta(node1Name, ""),
								},
								{
									Status: v1.NodeStatus{
										Phase: v1.NodeRunning,
									},
									ObjectMeta: newObjectMeta(node2Name, ""),
								},
							},
						})

					fakeOVN.controller.WatchEgressFirewall()

					ipv4ACL := libovsdbops.BuildACL(
						"",
						t.DirectionToLPort,
						t.EgressFirewallStartPriority,
						"(ip4.dst == 1.2.3.5/23) && ip4.src == $a10481622940199974102 && ((tcp && ( tcp.dst == 100 ))) && ip4.dst != 10.128.0.0/14",
						nbdb.ACLActionAllow,
						"",
						"",
						false,
						map[string]string{egressFirewallACLExtIdKey: "namespace1"},
					)
					ipv4ACL.UUID = libovsdbops.BuildNamedUUID()

					// new ACL will be added to the port group
					clusterPortGroup.ACLs = []string{ipv4ACL.UUID}
					expectedDatabaseState := append(initialData, ipv4ACL)

					gomega.Expect(fakeOVN.nbClient).To(libovsdbtest.HaveData(expectedDatabaseState))

					err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Delete(context.TODO(), egressFirewall.Name, *metav1.NewDeleteOptions(0))
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// ACL should be removed from the port group egfw is deleted
					clusterPortGroup.ACLs = []string{}
					// this ACL will be deleted when test server starts deleting dereferenced ACLs
					expectedDatabaseState = append(initialData, ipv4ACL)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			})
			ginkgo.It(fmt.Sprintf("correctly updates an egressfirewall, gateway mode %s", gwMode), func() {
				app.Action = func(ctx *cli.Context) error {
					namespace1 := *newNamespace("namespace1")
					egressFirewall := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Allow",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})
					egressFirewall1 := newEgressFirewallObject("default", namespace1.Name, []egressfirewallapi.EgressFirewallRule{
						{
							Type: "Deny",
							To: egressfirewallapi.EgressFirewallDestination{
								CIDRSelector: "1.2.3.4/23",
							},
						},
					})

					fakeOVN.startWithDBSetup(dbSetup,
						&egressfirewallapi.EgressFirewallList{
							Items: []egressfirewallapi.EgressFirewall{
								*egressFirewall,
							},
						},
						&v1.NamespaceList{
							Items: []v1.Namespace{
								namespace1,
							},
						},
						&v1.NodeList{
							Items: []v1.Node{
								{
									Status: v1.NodeStatus{
										Phase: v1.NodeRunning,
									},
									ObjectMeta: newObjectMeta(node1Name, ""),
								},
							},
						})

					fakeOVN.controller.WatchNamespaces()
					fakeOVN.controller.WatchEgressFirewall()

					ipv4ACL := libovsdbops.BuildACL(
						"",
						t.DirectionToLPort,
						t.EgressFirewallStartPriority,
						"(ip4.dst == 1.2.3.4/23) && ip4.src == $a10481622940199974102 && ip4.dst != 10.128.0.0/14",
						nbdb.ACLActionAllow,
						"",
						"",
						false,
						map[string]string{egressFirewallACLExtIdKey: "namespace1"},
					)
					ipv4ACL.UUID = libovsdbops.BuildNamedUUID()

					// new ACL will be added to the port group
					clusterPortGroup.ACLs = []string{ipv4ACL.UUID}
					expectedDatabaseState := append(initialData, ipv4ACL)
					gomega.Expect(fakeOVN.nbClient).To(libovsdbtest.HaveData(expectedDatabaseState))

					_, err := fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					_, err = fakeOVN.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall1.Namespace).Update(context.TODO(), egressFirewall1, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					// egress firewall is updated by deleting and creating from scratch.
					// since old acl won't be garbage-collected by the test server, it will stay,
					// but won't be referenced from the switch
					ipv4ACLStale := libovsdbops.BuildACL(
						"",
						t.DirectionToLPort,
						t.EgressFirewallStartPriority,
						"(ip4.dst == 1.2.3.4/23) && ip4.src == $a10481622940199974102 && ip4.dst != 10.128.0.0/14",
						nbdb.ACLActionAllow,
						"",
						"",
						false,
						map[string]string{"egressFirewall": "namespace1"},
					)
					ipv4ACLStale.UUID = "ipv4ACLStale-UUID"
					ipv4ACL.Action = nbdb.ACLActionDrop
					expectedDatabaseState = append(expectedDatabaseState, ipv4ACLStale)
					gomega.Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))

					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

			})
		}
	})
})

var _ = ginkgo.Describe("OVN test basic functions", func() {

	ginkgo.It("computes correct L4Match", func() {
		type testcase struct {
			ports         []egressfirewallapi.EgressFirewallPort
			expectedMatch string
		}
		testcases := []testcase{
			{
				ports: []egressfirewallapi.EgressFirewallPort{
					{
						Protocol: "TCP",
						Port:     100,
					},
				},
				expectedMatch: "((tcp && ( tcp.dst == 100 )))",
			},
			{
				ports: []egressfirewallapi.EgressFirewallPort{
					{
						Protocol: "TCP",
						Port:     100,
					},
					{
						Protocol: "UDP",
					},
				},
				expectedMatch: "((udp) || (tcp && ( tcp.dst == 100 )))",
			},
			{
				ports: []egressfirewallapi.EgressFirewallPort{
					{
						Protocol: "TCP",
						Port:     100,
					},
					{
						Protocol: "SCTP",
						Port:     13,
					},
					{
						Protocol: "TCP",
						Port:     102,
					},
					{
						Protocol: "UDP",
						Port:     400,
					},
				},
				expectedMatch: "((udp && ( udp.dst == 400 )) || (tcp && ( tcp.dst == 100 || tcp.dst == 102 )) || (sctp && ( sctp.dst == 13 )))",
			},
		}
		for _, test := range testcases {
			l4Match := egressGetL4Match(test.ports)
			gomega.Expect(test.expectedMatch).To(gomega.Equal(l4Match))
		}
	})
	ginkgo.It("computes correct match function", func() {
		type testcase struct {
			internalCIDRs []string
			ipv4source    string
			ipv6source    string
			ipv4Mode      bool
			ipv6Mode      bool
			destinations  []matchTarget
			ports         []egressfirewallapi.EgressFirewallPort
			output        string
		}
		testcases := []testcase{
			{
				internalCIDRs: []string{"10.128.0.0/14"},
				ipv4source:    "testv4",
				ipv6source:    "",
				ipv4Mode:      true,
				ipv6Mode:      false,
				destinations:  []matchTarget{{matchKindV4CIDR, "1.2.3.4/32"}},
				ports:         nil,
				output:        "(ip4.dst == 1.2.3.4/32) && ip4.src == $testv4 && ip4.dst != 10.128.0.0/14",
			},
			{
				internalCIDRs: []string{"10.128.0.0/14", "2002:0:0:1234::/64"},
				ipv4source:    "testv4",
				ipv6source:    "testv6",
				ipv4Mode:      true,
				ipv6Mode:      true,
				destinations:  []matchTarget{{matchKindV4CIDR, "1.2.3.4/32"}},
				ports:         nil,
				output:        "(ip4.dst == 1.2.3.4/32) && (ip4.src == $testv4 || ip6.src == $testv6) && ip4.dst != 10.128.0.0/14 && ip6.dst != 2002:0:0:1234::/64",
			},
			{
				internalCIDRs: []string{"10.128.0.0/14", "2002:0:0:1234::/64"},
				ipv4source:    "testv4",
				ipv6source:    "testv6",
				ipv4Mode:      true,
				ipv6Mode:      true,
				destinations:  []matchTarget{{matchKindV4AddressSet, "destv4"}, {matchKindV6AddressSet, "destv6"}},
				ports:         nil,
				output:        "(ip4.dst == $destv4 || ip6.dst == $destv6) && (ip4.src == $testv4 || ip6.src == $testv6) && ip4.dst != 10.128.0.0/14 && ip6.dst != 2002:0:0:1234::/64",
			},
			{
				internalCIDRs: []string{"10.128.0.0/14"},
				ipv4source:    "testv4",
				ipv6source:    "",
				ipv4Mode:      true,
				ipv6Mode:      false,
				destinations:  []matchTarget{{matchKindV4AddressSet, "destv4"}, {matchKindV6AddressSet, ""}},
				ports:         nil,
				output:        "(ip4.dst == $destv4) && ip4.src == $testv4 && ip4.dst != 10.128.0.0/14",
			},
			{
				internalCIDRs: []string{"10.128.0.0/14", "2002:0:0:1234::/64"},
				ipv4source:    "testv4",
				ipv6source:    "testv6",
				ipv4Mode:      true,
				ipv6Mode:      true,
				destinations:  []matchTarget{{matchKindV6CIDR, "2001::/64"}},
				ports:         nil,
				output:        "(ip6.dst == 2001::/64) && (ip4.src == $testv4 || ip6.src == $testv6) && ip4.dst != 10.128.0.0/14 && ip6.dst != 2002:0:0:1234::/64",
			},
			{
				internalCIDRs: []string{"2002:0:0:1234::/64"},
				ipv4source:    "",
				ipv6source:    "testv6",
				ipv4Mode:      false,
				ipv6Mode:      true,
				destinations:  []matchTarget{{matchKindV6AddressSet, "destv6"}},
				ports:         nil,
				output:        "(ip6.dst == $destv6) && ip6.src == $testv6 && ip6.dst != 2002:0:0:1234::/64",
			},
		}

		for _, tc := range testcases {
			config.IPv4Mode = tc.ipv4Mode
			config.IPv6Mode = tc.ipv6Mode
			subnets := []config.CIDRNetworkEntry{}
			for _, clusterCIDR := range tc.internalCIDRs {
				_, cidr, _ := net.ParseCIDR(clusterCIDR)
				subnets = append(subnets, config.CIDRNetworkEntry{CIDR: cidr})
			}
			config.Default.ClusterSubnets = subnets

			config.Gateway.Mode = config.GatewayModeShared
			matchExpression := generateMatch(tc.ipv4source, tc.ipv6source, tc.destinations, tc.ports)
			gomega.Expect(tc.output).To(gomega.Equal(matchExpression))
		}
	})
	ginkgo.It("correctly parses egressFirewallRules", func() {
		type testcase struct {
			egressFirewallRule egressfirewallapi.EgressFirewallRule
			id                 int
			err                bool
			errOutput          string
			output             egressFirewallRule
		}
		testcases := []testcase{
			{
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "1.2.3.4/32"},
				},
				id:  1,
				err: false,
				output: egressFirewallRule{
					id:     1,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{cidrSelector: "1.2.3.4/32"},
				},
			},
			{
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "1.2.3./32"},
				},
				id:        1,
				err:       true,
				errOutput: "invalid CIDR address: 1.2.3./32",
				output:    egressFirewallRule{},
			},
			{
				egressFirewallRule: egressfirewallapi.EgressFirewallRule{
					Type: egressfirewallapi.EgressFirewallRuleAllow,
					To:   egressfirewallapi.EgressFirewallDestination{CIDRSelector: "2002::1234:abcd:ffff:c0a8:101/64"},
				},
				id:  2,
				err: false,
				output: egressFirewallRule{
					id:     2,
					access: egressfirewallapi.EgressFirewallRuleAllow,
					to:     destination{cidrSelector: "2002::1234:abcd:ffff:c0a8:101/64"},
				},
			},
		}
		for _, tc := range testcases {
			output, err := newEgressFirewallRule(tc.egressFirewallRule, tc.id)
			if tc.err == true {
				gomega.Expect(err).To(gomega.HaveOccurred())
				gomega.Expect(tc.errOutput).To(gomega.Equal(err.Error()))
			} else {
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(tc.output).To(gomega.Equal(*output))
			}
		}
	})
})

//helper functions to help test egressfirewallDNS

// Create an EgressDNS object without the Sync function
// To make it easier to mock EgressFirewall functionality create an egressFirewall
// without the go routine of the sync function

//GetDNSEntryForTest Gets a dnsEntry from a EgressDNS object for testing
func (e *EgressDNS) GetDNSEntryForTest(dnsName string) (map[string]struct{}, []net.IP, addressset.AddressSet, error) {
	if e.dnsEntries[dnsName] == nil {
		return nil, nil, nil, fmt.Errorf("there is no dnsEntry for dnsName: %s", dnsName)
	}
	return e.dnsEntries[dnsName].namespaces, e.dnsEntries[dnsName].dnsResolves, e.dnsEntries[dnsName].dnsAddressSet, nil
}
