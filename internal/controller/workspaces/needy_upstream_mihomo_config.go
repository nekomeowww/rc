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

package workspaces

import (
	"encoding/hex"
	"fmt"
	"slices"

	"github.com/nekomeowww/xo"
	"sigs.k8s.io/yaml"
)

// reservedRanges are the special-purpose IPv4 ranges that no actor reaches
// through the upstream, in addition to the cluster's ranges.
var reservedRanges = []string{
	"0.0.0.0/8",
	"10.0.0.0/8",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.0.0.0/24",
	"192.0.2.0/24",
	"192.168.0.0/16",
	"198.18.0.0/15",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"224.0.0.0/3",
}

// publicResolvers are the upstream's own resolvers and the DNS-over-HTTPS
// endpoints, which an actor may not use to resolve around the upstream.
var publicResolvers = []string{
	"1.1.1.1/32",
	"1.0.0.1/32",
	"8.8.8.8/32",
	"8.8.4.4/32",
	"9.9.9.9/32",
	"149.112.112.112/32",
}

var dohDomains = []string{
	"cloudflare-dns.com",
	"dns.google",
	"doh.pub",
	"dns.quad9.net",
}

// needyUpstreamBlockedRanges are the ranges that the upstream refuses and that its
// NetworkPolicy keeps it from dialing: the cluster's, then the reserved ones.
func needyUpstreamBlockedRanges(clusterRanges []string) []string {
	ranges := slices.Clone(clusterRanges)
	for _, reserved := range reservedRanges {
		if !slices.Contains(ranges, reserved) {
			ranges = append(ranges, reserved)
		}
	}

	return ranges
}

// newNeedyUpstreamPassword returns a random password for the actor user.
func newNeedyUpstreamPassword() (string, error) {
	secret, err := xo.RandBytes(24)
	if err != nil {
		return "", fmt.Errorf("generate upstream password: %w", err)
	}

	return hex.EncodeToString(secret), nil
}

// mihomoConfig renders the configuration of a Mihomo managed upstream.
//
// Every IP-CIDR rule omits no-resolve: the first one makes Mihomo resolve a
// socks5h name and match the address it resolves to, so a name that points
// into the cluster is refused. A name that does not resolve is refused too;
// it would otherwise reach the final DIRECT. Mihomo matches one resolved
// address but dials every one, so its NetworkPolicy must refuse the same
// ranges.
func mihomoConfig(password string, clusterRanges []string) ([]byte, error) {
	blocked := needyUpstreamBlockedRanges(clusterRanges)
	rules := make([]string, 0, len(blocked)+len(publicResolvers)+len(dohDomains)+2)
	for _, blocked := range blocked {
		rules = append(rules, "IP-CIDR,"+blocked+",REJECT")
	}
	rules = append(rules, "NOT,((IP-CIDR,0.0.0.0/0)),REJECT")
	for _, resolver := range publicResolvers {
		rules = append(rules, "IP-CIDR,"+resolver+",REJECT")
	}
	for _, domain := range dohDomains {
		rules = append(rules, "DOMAIN-SUFFIX,"+domain+",REJECT")
	}
	rules = append(rules, "MATCH,DIRECT")
	resolvers := []string{
		"1.1.1.1",
		"8.8.8.8",
	}
	config := mihomoConfiguration{
		LogLevel:    "info",
		Mode:        "rule",
		IPv6:        false,
		AllowLAN:    true,
		BindAddress: "*",
		Profile: mihomoProfile{
			StoreSelected: false,
			StoreFakeIP:   false,
		},
		Listeners: []mihomoListener{
			{
				Name:   "socks-in",
				Type:   "socks",
				Listen: "0.0.0.0",
				Port:   needyUpstreamPort,
				UDP:    false,
				Users: []mihomoAccount{
					{
						Username: needyUpstreamActorUser,
						Password: password,
					},
				},
			},
		},
		DNS: mihomoDNS{
			Enable:            true,
			IPv6:              false,
			EnhancedMode:      "redir-host",
			UseHosts:          false,
			UseSystemHosts:    false,
			DefaultNameserver: resolvers,
			Nameserver:        resolvers,
		},
		Rules: rules,
	}
	data, err := yaml.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("render Mihomo config: %w", err)
	}

	return data, nil
}

// mihomoConfiguration is the part of Mihomo's configuration that the
// managed upstream sets.
type mihomoConfiguration struct {
	LogLevel    string           `json:"log-level"`
	Mode        string           `json:"mode"`
	IPv6        bool             `json:"ipv6"`
	AllowLAN    bool             `json:"allow-lan"`
	BindAddress string           `json:"bind-address"`
	Profile     mihomoProfile    `json:"profile"`
	Listeners   []mihomoListener `json:"listeners"`
	DNS         mihomoDNS        `json:"dns"`
	Rules       []string         `json:"rules"`
}

type mihomoProfile struct {
	StoreSelected bool `json:"store-selected"`
	StoreFakeIP   bool `json:"store-fake-ip"`
}

type mihomoListener struct {
	Name   string          `json:"name"`
	Type   string          `json:"type"`
	Listen string          `json:"listen"`
	Port   int             `json:"port"`
	UDP    bool            `json:"udp"`
	Users  []mihomoAccount `json:"users"`
}

type mihomoAccount struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type mihomoDNS struct {
	Enable            bool     `json:"enable"`
	IPv6              bool     `json:"ipv6"`
	EnhancedMode      string   `json:"enhanced-mode"`
	UseHosts          bool     `json:"use-hosts"`
	UseSystemHosts    bool     `json:"use-system-hosts"`
	DefaultNameserver []string `json:"default-nameserver"`
	Nameserver        []string `json:"nameserver"`
}
