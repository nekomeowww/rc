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

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	sshv1alpha1 "github.com/nekomeowww/rc/apis/sdk/go/datad/ssh/v1alpha1"
)

func generateTestPubKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("convert ssh public key: %v", err)
	}
	return sshPub
}

func TestPluginAuthenticateOverGRPC(t *testing.T) {
	goodKey := generateTestPubKey(t)
	badKey := generateTestPubKey(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	sshv1alpha1.RegisterSSHAuthServiceServer(srv, &pluginServer{
		routes: []routeEntry{
			{
				username:   "alice",
				authority:  "ws-1.example.com",
				namespace:  "team-a",
				workspace:  "sandbox-1",
				userID:     "uid-alice",
				publicKeys: []ssh.PublicKey{goodKey},
			},
		},
	})
	go func() {
		_ = srv.Serve(ln)
	}()
	t.Cleanup(srv.GracefulStop)

	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial grpc: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	client := sshv1alpha1.NewSSHAuthServiceClient(conn)
	ctx := context.Background()

	// Valid user + authority + key succeeds.
	resp, err := client.Authenticate(ctx, &sshv1alpha1.AuthenticateRequest{
		Username:             "alice",
		Authority:            "ws-1.example.com",
		Method:               sshv1alpha1.SSHAuthMethod_SSH_AUTH_METHOD_PUBLIC_KEY,
		PublicKey:            strings.TrimSpace(string(ssh.MarshalAuthorizedKey(goodKey))),
		PublicKeyFingerprint: ssh.FingerprintSHA256(goodKey),
	})
	if err != nil {
		t.Fatalf("authenticate valid request: %v", err)
	}
	if !resp.GetAllowed() || resp.GetNamespace() != "team-a" || resp.GetWorkspace() != "sandbox-1" || resp.GetUserId() != "uid-alice" {
		t.Fatalf("unexpected response: %+v", resp)
	}

	// Wrong key is denied.
	denied, err := client.Authenticate(ctx, &sshv1alpha1.AuthenticateRequest{
		Username:             "alice",
		Authority:            "ws-1.example.com",
		Method:               sshv1alpha1.SSHAuthMethod_SSH_AUTH_METHOD_PUBLIC_KEY,
		PublicKey:            strings.TrimSpace(string(ssh.MarshalAuthorizedKey(badKey))),
		PublicKeyFingerprint: ssh.FingerprintSHA256(badKey),
	})
	if err != nil {
		t.Fatalf("authenticate wrong key: %v", err)
	}
	if denied.GetAllowed() {
		t.Fatalf("expected wrong key to be denied")
	}

	// Wrong SNI authority is denied.
	wrongHost, err := client.Authenticate(ctx, &sshv1alpha1.AuthenticateRequest{
		Username:             "alice",
		Authority:            "ws-2.example.com",
		Method:               sshv1alpha1.SSHAuthMethod_SSH_AUTH_METHOD_PUBLIC_KEY,
		PublicKey:            strings.TrimSpace(string(ssh.MarshalAuthorizedKey(goodKey))),
		PublicKeyFingerprint: ssh.FingerprintSHA256(goodKey),
	})
	if err != nil {
		t.Fatalf("authenticate wrong authority: %v", err)
	}
	if wrongHost.GetAllowed() {
		t.Fatalf("expected wrong authority to be denied")
	}
}
