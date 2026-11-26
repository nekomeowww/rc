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

package needy

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"

	"sigs.k8s.io/yaml"
)

// The managed upstream: one filtering Mihomo SOCKS5 proxy per namespace.
const (
	// UpstreamName names its Deployment, Service, and NetworkPolicy.
	UpstreamName = "needy-upstream"
	// UpstreamConfigSecretName holds its config.yaml, with the password.
	UpstreamConfigSecretName = "needy-upstream-config"
	// UpstreamCredentialsSecretName holds USERNAME:PASSWORD for actor runners.
	UpstreamCredentialsSecretName = "needy-upstream-actor"
	// UpstreamConfigKey is the config in UpstreamConfigSecretName.
	UpstreamConfigKey = "config.yaml"
	// UpstreamPort is its SOCKS5 port.
	UpstreamPort = 1080
	// UpstreamActorUser is the user that actor runners log in as.
	UpstreamActorUser = "actor"
	// ComponentUpstream is the ComponentLabel value of its objects.
	ComponentUpstream = "upstream"
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

// UpstreamBlockedRanges are the ranges that the upstream refuses and that its
// NetworkPolicy keeps it from dialing: the cluster's, then the reserved ones.
func UpstreamBlockedRanges(clusterRanges []string) []string {
	ranges := slices.Clone(clusterRanges)
	for _, reserved := range reservedRanges {
		if !slices.Contains(ranges, reserved) {
			ranges = append(ranges, reserved)
		}
	}

	return ranges
}

// NewUpstreamPassword returns a random password for the actor user.
func NewUpstreamPassword() (string, error) {
	secret := make([]byte, 24)
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("generate upstream password: %w", err)
	}

	return hex.EncodeToString(secret), nil
}

// UpstreamConfig renders the upstream's Mihomo configuration.
//
// Every IP-CIDR rule omits no-resolve: the first one makes Mihomo resolve a
// socks5h name and match the address it resolves to, so a name that points
// into the cluster is refused. A name that does not resolve is refused too;
// it would otherwise reach the final DIRECT. Mihomo matches one resolved
// address but dials every one, so its NetworkPolicy must refuse the same
// ranges.
func UpstreamConfig(password string, clusterRanges []string) ([]byte, error) {
	blocked := UpstreamBlockedRanges(clusterRanges)
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
	config := map[string]any{
		"log-level":    "info",
		"mode":         "rule",
		"ipv6":         false,
		"allow-lan":    true,
		"bind-address": "*",
		"profile": map[string]any{
			"store-selected": false,
			"store-fake-ip":  false,
		},
		"listeners": []any{
			map[string]any{
				"name":   "socks-in",
				"type":   "socks",
				"listen": "0.0.0.0",
				"port":   UpstreamPort,
				"udp":    false,
				"users": []any{
					map[string]any{
						"username": UpstreamActorUser,
						"password": password,
					},
				},
			},
		},
		"dns": map[string]any{
			"enable":             true,
			"ipv6":               false,
			"enhanced-mode":      "redir-host",
			"use-hosts":          false,
			"use-system-hosts":   false,
			"default-nameserver": resolvers,
			"nameserver":         resolvers,
		},
		"rules": rules,
	}
	data, err := yaml.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("render upstream config: %w", err)
	}

	return data, nil
}
