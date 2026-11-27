package kafka

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/IBM/sarama"
	"github.com/xdg-go/scram"
)

type testPKI struct {
	caFile         string
	wrongCAFile    string
	clientCertFile string
	clientKeyFile  string
	serverTLS      *tls.Config
}

func generateTestPKI(t *testing.T, requireClientCert bool) testPKI {
	t.Helper()
	dir := t.TempDir()

	genCA := func(cn string) (*x509.Certificate, *rsa.PrivateKey, []byte) {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("GenerateKey: %v", err)
		}
		tmpl := &x509.Certificate{
			SerialNumber:          big.NewInt(1),
			Subject:               pkix.Name{CommonName: cn},
			NotBefore:             time.Now().Add(-time.Hour),
			NotAfter:              time.Now().Add(24 * time.Hour),
			KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
			BasicConstraintsValid: true,
			IsCA:                  true,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			t.Fatalf("CreateCertificate CA: %v", err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatalf("ParseCertificate CA: %v", err)
		}
		caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		return cert, key, caPEM
	}

	caCert, caKey, caPEM := genCA("test-kafka-ca")
	_, _, wrongCAPEM := genCA("wrong-kafka-ca")

	issueLeaf := func(cn string, serial int64, extUsage x509.ExtKeyUsage, ips []net.IP, dns []string) (tls.Certificate, []byte, []byte) {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("GenerateKey leaf: %v", err)
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial),
			Subject:      pkix.Name{CommonName: cn},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(24 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			ExtKeyUsage:  []x509.ExtKeyUsage{extUsage},
			IPAddresses:  ips,
			DNSNames:     dns,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
		if err != nil {
			t.Fatalf("CreateCertificate leaf %s: %v", cn, err)
		}
		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
		pair, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			t.Fatalf("X509KeyPair %s: %v", cn, err)
		}
		return pair, certPEM, keyPEM
	}

	serverPair, _, _ := issueLeaf("127.0.0.1", 2, x509.ExtKeyUsageServerAuth, []net.IP{net.ParseIP("127.0.0.1")}, []string{"localhost"})
	_, clientCertPEM, clientKeyPEM := issueLeaf("kafka-client", 3, x509.ExtKeyUsageClientAuth, nil, nil)

	caFile := filepath.Join(dir, "ca.pem")
	wrongCAFile := filepath.Join(dir, "wrong-ca.pem")
	clientCertFile := filepath.Join(dir, "client.pem")
	clientKeyFile := filepath.Join(dir, "client-key.pem")
	for path, data := range map[string][]byte{
		caFile:         caPEM,
		wrongCAFile:    wrongCAPEM,
		clientCertFile: clientCertPEM,
		clientKeyFile:  clientKeyPEM,
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("WriteFile %s: %v", path, err)
		}
	}

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)

	clientAuth := tls.NoClientCert
	if requireClientCert {
		clientAuth = tls.RequireAndVerifyClientCert
	}

	return testPKI{
		caFile:         caFile,
		wrongCAFile:    wrongCAFile,
		clientCertFile: clientCertFile,
		clientKeyFile:  clientKeyFile,
		serverTLS: &tls.Config{
			Certificates: []tls.Certificate{serverPair},
			ClientCAs:    pool,
			ClientAuth:   clientAuth,
			MinVersion:   tls.VersionTLS12,
		},
	}
}

func mockApiVersionsWithSASL(t *testing.T) *sarama.MockApiVersionsResponse {
	t.Helper()
	return sarama.NewMockApiVersionsResponse(t).SetApiKeys([]sarama.ApiVersionsResponseKey{
		{ApiKey: 0, MinVersion: 0, MaxVersion: 8},  // Produce
		{ApiKey: 1, MinVersion: 0, MaxVersion: 11}, // Fetch
		{ApiKey: 2, MinVersion: 0, MaxVersion: 5},  // ListOffsets
		{ApiKey: 3, MinVersion: 0, MaxVersion: 10}, // Metadata
		{ApiKey: 8, MinVersion: 0, MaxVersion: 7},  // OffsetCommit
		{ApiKey: 9, MinVersion: 0, MaxVersion: 7},  // OffsetFetch
		{ApiKey: 10, MinVersion: 0, MaxVersion: 3}, // FindCoordinator
		{ApiKey: 11, MinVersion: 0, MaxVersion: 5}, // JoinGroup
		{ApiKey: 12, MinVersion: 0, MaxVersion: 3}, // Heartbeat
		{ApiKey: 13, MinVersion: 0, MaxVersion: 4}, // LeaveGroup
		{ApiKey: 14, MinVersion: 0, MaxVersion: 3}, // SyncGroup
		{ApiKey: 17, MinVersion: 0, MaxVersion: 1}, // SaslHandshake
		{ApiKey: 22, MinVersion: 0, MaxVersion: 4}, // InitProducerID
		{ApiKey: 36, MinVersion: 0, MaxVersion: 2}, // SaslAuthenticate
	})
}

func TestAuthTLSSASL_MTLSAndPLAINMockBrokerConsumerGroup(t *testing.T) {
	const topic, group, member = "tls-topic", "mtls-group", "mtls-member"
	const generation = 3
	pki := generateTestPKI(t, true)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", pki.serverTLS)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}

	mb := sarama.NewMockBrokerListener(t, 1, listener)
	defer mb.Close()

	mb.SetHandlerByMap(map[string]sarama.MockResponse{
		"ApiVersionsRequest":      mockApiVersionsWithSASL(t),
		"SaslHandshakeRequest":    sarama.NewMockSaslHandshakeResponse(t).SetEnabledMechanisms([]string{sarama.SASLTypePlaintext}),
		"SaslAuthenticateRequest": sarama.NewMockSaslAuthenticateResponse(t),
		"MetadataRequest": sarama.NewMockMetadataResponse(t).
			SetBroker(mb.Addr(), mb.BrokerID()).
			SetController(mb.BrokerID()).
			SetLeader(topic, 0, mb.BrokerID()),
		"InitProducerIDRequest": sarama.NewMockInitProducerIDResponse(t).SetProducerID(42),
		"ProduceRequest":        sarama.NewMockProduceResponse(t),
		"FindCoordinatorRequest": sarama.NewMockFindCoordinatorResponse(t).
			SetCoordinator(sarama.CoordinatorGroup, group, mb),
		"JoinGroupRequest": sarama.NewMockJoinGroupResponse(t).
			SetMemberId(member).SetGenerationId(generation).SetGroupProtocol(sarama.RoundRobinBalanceStrategyName),
		"SyncGroupRequest": sarama.NewMockSyncGroupResponse(t).
			SetMemberAssignment(&sarama.ConsumerGroupMemberAssignment{Topics: map[string][]int32{topic: {0}}}),
		"HeartbeatRequest": sarama.NewMockHeartbeatResponse(t),
		"OffsetFetchRequest": sarama.NewMockOffsetFetchResponse(t).
			SetOffset(group, topic, 0, 7, "", sarama.ErrNoError),
		"OffsetRequest": sarama.NewMockOffsetResponse(t).
			SetOffset(topic, 0, sarama.OffsetOldest, 0).SetOffset(topic, 0, sarama.OffsetNewest, 8),
		"FetchRequest": sarama.NewMockFetchResponse(t, 1).
			SetMessage(topic, 0, 7, sarama.StringEncoder("v1")).SetHighWaterMark(topic, 0, 8),
		"OffsetCommitRequest": sarama.NewMockOffsetCommitResponse(t),
		"LeaveGroupRequest":   sarama.NewMockLeaveGroupResponse(t),
	})

	brokerCfg := DefaultBrokerConfig()
	brokerCfg.Brokers = []string{mb.Addr()}
	brokerCfg.DialTimeout = 3 * time.Second
	brokerCfg.ReadTimeout = 3 * time.Second
	brokerCfg.WriteTimeout = 3 * time.Second
	brokerCfg.Auth.TLS = TLSConfig{
		Enabled:  true,
		CAFile:   pki.caFile,
		CertFile: pki.clientCertFile,
		KeyFile:  pki.clientKeyFile,
	}
	brokerCfg.Auth.SASL = SASLConfig{
		Enabled:   true,
		Mechanism: "PLAIN",
		Username:  "alice",
		Password:  "s3cr3t",
	}

	prod, err := NewProducer(brokerCfg, DefaultProducerConfig())
	if err != nil {
		t.Fatalf("NewProducer over mTLS+SASL/PLAIN: %v", err)
	}
	if err := prod.Publish(context.Background(), topic, []byte("k1"), []byte("v1")); err != nil {
		_ = prod.Close()
		t.Fatalf("Publish over mTLS+SASL/PLAIN: %v", err)
	}
	if err := prod.Close(); err != nil {
		t.Fatalf("prod.Close: %v", err)
	}

	producerExchanges := len(mb.History())
	cc := DefaultConsumerConfig()
	cc.GroupID = group
	cc.CommitMode = CommitSync
	cons, err := NewConsumer(brokerCfg, cc, discardLogger())
	if err != nil {
		t.Fatalf("NewConsumer over mTLS+SASL/PLAIN: %v", err)
	}
	defer cons.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var received *Message
	var ready bool
	if err := cons.Subscribe(Subscription{
		Topic:       topic,
		RetryPolicy: DefaultRetryPolicy(),
		Handler: func(_ context.Context, msg *Message) error {
			received = msg
			ready = cons.Health().Ready
			cancel()
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cons.Run(ctx); err != nil {
		t.Fatalf("authenticated consumer Run: %v", err)
	}
	if !ready || received == nil || received.Topic != topic || received.Partition != 0 || received.Offset != 7 || string(received.Value) != "v1" {
		t.Fatalf("expected ready consumer to receive mock record at %s/0@7, ready=%t record=%+v", topic, ready, received)
	}
	if health := cons.Health(); health.State != ConsumerStateStopped || health.Ready || health.LastSuccessfulAt.IsZero() {
		t.Fatalf("expected successful processing and clean shutdown, got %+v", health)
	}
	if err := cons.Close(); err != nil {
		t.Fatalf("cons.Close: %v", err)
	}

	var authenticated, joined, synced, fetched, committed, left bool
	for _, exchange := range mb.History()[producerExchanges:] {
		switch req := exchange.Request.(type) {
		case *sarama.SaslAuthenticateRequest:
			authenticated = true
		case *sarama.JoinGroupRequest:
			joined = authenticated && req.GroupId == group
		case *sarama.SyncGroupRequest:
			synced = joined && req.GroupId == group && req.MemberId == member && req.GenerationId == generation
		case *sarama.FetchRequest:
			fetched = fetched || synced
		case *sarama.OffsetCommitRequest:
			offset, _, err := req.Offset(topic, 0)
			response, ok := exchange.Response.(*sarama.OffsetCommitResponse)
			if err != nil || offset != 8 || req.ConsumerID != member || req.ConsumerGroup != group ||
				req.ConsumerGroupGeneration != generation || !ok {
				t.Fatalf("unexpected authenticated commit: offset=%d err=%v request=%+v response=%+v", offset, err, req, exchange.Response)
			}
			if code, present := response.Errors[topic][0]; !present || code != sarama.ErrNoError {
				t.Fatalf("mock broker did not acknowledge the authenticated commit: %+v", response.Errors)
			}
			committed = fetched
		case *sarama.LeaveGroupRequest:
			left = committed && req.GroupId == group
		}
	}
	if !left {
		t.Fatalf("incomplete consumer exchange: authenticated=%t joined=%t synced=%t fetched=%t committed=%t left=%t",
			authenticated, joined, synced, fetched, committed, left)
	}
}

func TestAuthTLSSASL_RejectsMissingClientCertAndUntrustedCA(t *testing.T) {
	pki := generateTestPKI(t, true)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", pki.serverTLS)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	// Use a throwaway testing.T on MockBroker because failed TLS handshakes log socket errors on the broker side.
	mb := sarama.NewMockBrokerListener(&testing.T{}, 1, listener)
	defer mb.Close()

	baseBrokerCfg := DefaultBrokerConfig()
	baseBrokerCfg.Brokers = []string{mb.Addr()}
	baseBrokerCfg.DialTimeout = 2 * time.Second
	baseBrokerCfg.ReadTimeout = 2 * time.Second
	baseBrokerCfg.WriteTimeout = 2 * time.Second

	t.Run("missing client certificate fails mTLS", func(t *testing.T) {
		cfg := baseBrokerCfg
		cfg.Auth.TLS = TLSConfig{
			Enabled: true,
			CAFile:  pki.caFile,
		}
		if _, err := NewProducer(cfg, DefaultProducerConfig()); err == nil {
			t.Fatal("expected mTLS failure when client certificate is missing")
		}
	})

	t.Run("untrusted server CA rejected", func(t *testing.T) {
		cfg := baseBrokerCfg
		cfg.Auth.TLS = TLSConfig{
			Enabled:  true,
			CAFile:   pki.wrongCAFile,
			CertFile: pki.clientCertFile,
			KeyFile:  pki.clientKeyFile,
		}
		if _, err := NewProducer(cfg, DefaultProducerConfig()); err == nil {
			t.Fatal("expected TLS failure when server cert is not signed by configured CA")
		}
	})
}

func TestAuthTLSSASL_RejectsFailedSASLAuthenticationOverTLS(t *testing.T) {
	pki := generateTestPKI(t, false)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", pki.serverTLS)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	mb := sarama.NewMockBrokerListener(&testing.T{}, 1, listener)
	defer mb.Close()

	mb.SetHandlerByMap(map[string]sarama.MockResponse{
		"ApiVersionsRequest":   mockApiVersionsWithSASL(t),
		"SaslHandshakeRequest": sarama.NewMockSaslHandshakeResponse(t).SetEnabledMechanisms([]string{sarama.SASLTypePlaintext}),
		"SaslAuthenticateRequest": sarama.NewMockSaslAuthenticateResponse(t).
			SetError(sarama.ErrSASLAuthenticationFailed),
	})

	cfg := DefaultBrokerConfig()
	cfg.Brokers = []string{mb.Addr()}
	cfg.DialTimeout = 2 * time.Second
	cfg.ReadTimeout = 2 * time.Second
	cfg.WriteTimeout = 2 * time.Second
	cfg.Auth.TLS = TLSConfig{Enabled: true, CAFile: pki.caFile}
	cfg.Auth.SASL = SASLConfig{Enabled: true, Mechanism: "PLAIN", Username: "bad", Password: "bad"}

	_, err = NewProducer(cfg, DefaultProducerConfig())
	if err == nil {
		t.Fatal("expected NewProducer to fail when SASL authentication returns an error")
	}
	if !errors.Is(err, sarama.ErrSASLAuthenticationFailed) {
		t.Fatalf("expected ErrSASLAuthenticationFailed, got: %v", err)
	}
}

func TestAuthTLSSASL_SCRAMConversationExchange(t *testing.T) {
	for _, tc := range []struct {
		mechanism string
		hashFn    scram.HashGeneratorFcn
	}{
		{mechanism: "SCRAM-SHA-256", hashFn: scram.SHA256},
		{mechanism: "SCRAM-SHA-512", hashFn: scram.SHA512},
	} {
		t.Run(tc.mechanism, func(t *testing.T) {
			brokerCfg := DefaultBrokerConfig()
			brokerCfg.Brokers = []string{"127.0.0.1:9092"}
			brokerCfg.Auth.TLS = TLSConfig{Enabled: true, InsecureSkipVerify: true}
			brokerCfg.Auth.SASL = SASLConfig{
				Enabled:   true,
				Mechanism: tc.mechanism,
				Username:  "scram-user",
				Password:  "scram-pass",
			}
			sc, err := buildBaseSaramaConfig(brokerCfg)
			if err != nil {
				t.Fatalf("buildSaramaConfig: %v", err)
			}
			if sc.Net.SASL.SCRAMClientGeneratorFunc == nil {
				t.Fatal("expected SCRAMClientGeneratorFunc to be populated")
			}

			clientSetup, err := tc.hashFn.NewClient("scram-user", "scram-pass", "")
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			stored := clientSetup.GetStoredCredentials(scram.KeyFactors{
				Salt:  "c2FsdC1ieXRlcw==",
				Iters: 4096,
			})
			server, err := tc.hashFn.NewServer(func(user string) (scram.StoredCredentials, error) {
				if user != "scram-user" {
					t.Fatalf("unexpected SCRAM username %q", user)
				}
				return stored, nil
			})
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}

			saramaClient := sc.Net.SASL.SCRAMClientGeneratorFunc()
			if err := saramaClient.Begin("scram-user", "scram-pass", ""); err != nil {
				t.Fatalf("saramaClient.Begin: %v", err)
			}
			serverConv := server.NewConversation()

			challenge := ""
			for !saramaClient.Done() {
				clientMsg, err := saramaClient.Step(challenge)
				if err != nil {
					t.Fatalf("saramaClient.Step: %v", err)
				}
				if clientMsg == "" && serverConv.Done() {
					break
				}
				serverMsg, err := serverConv.Step(clientMsg)
				if err != nil {
					t.Fatalf("serverConv.Step: %v", err)
				}
				challenge = serverMsg
			}
			if !serverConv.Valid() {
				t.Fatal("expected SCRAM server conversation to validate client proof")
			}
		})
	}
}
