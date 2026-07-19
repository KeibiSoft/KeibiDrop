package transport

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"

	pb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto"
	cpb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto/control"
)

// maxFrame is the gRPC message-size cap on both channels, matching KeibiDrop's tuned
// 16 MiB frame (its StreamFile chunk). The default is 4 MiB.
const maxFrame = 16 << 20

// controlService is the transport's own heartbeat responder, registered on every
// secured server so the Manager can Ping regardless of what app service runs alongside.
type controlService struct {
	cpb.UnimplementedControlServer
}

func (controlService) Ping(_ context.Context, _ *cpb.PingRequest) (*cpb.PingReply, error) {
	return &cpb.PingReply{}, nil
}

// This is the real thing: KeibiDrop's own post-quantum handshake, run over a QUIC
// stream, providing the security INSTEAD of relying on QUIC's TLS. secure.go is a
// simplified ephemeral copy; this imports KeibiDrop's pkg/crypto unchanged, so the
// seed-wrap, the ML-KEM-1024 encapsulation, the HKDF-SHA512 key derivation, the
// cipher negotiation, and the fingerprint are byte-identical to the product. That
// is what makes it a drop-in for Phase 3 and interoperable with a real KeibiDrop
// peer.
//
// Why a second layer at all, when Go 1.24+ already negotiates X25519MLKEM768 inside
// QUIC's TLS? Because TLS's post-quantum key exchange gives PQ *confidentiality* but
// authenticates with a signature certificate, and KeibiDrop's identity keys are KEM
// keys (X25519 + ML-KEM), not signature keys. The product authenticates a peer by
// KEM possession bound to a fingerprint, not by a cert. This handshake carries that
// exact model. The single-layer alternative (fold identity into the TLS cert) is
// analysed in docs/FINDINGS.md; it changes the identity model, so it is not a
// drop-in.

// Identity is a device's persistent post-quantum identity: an X25519 key and an
// ML-KEM-1024 key, the exact key material KeibiDrop stores in its encrypted identity
// file. The fingerprint is a hash over the two public keys and is the token peers
// verify out of band (over Signal, a QR code, in person).
type Identity struct {
	keys *kbc.OwnKeys
	fp   string
}

// NewIdentity generates a fresh identity (X25519 + ML-KEM-1024), like KeibiDrop's
// identity.create(). In the product this is loaded from disk instead of generated.
func NewIdentity() (*Identity, error) {
	kemPriv, kemPub, err := kbc.GenerateMLKEMKeypair()
	if err != nil {
		return nil, err
	}
	ecPriv, ecPub, err := kbc.GenerateX25519Keypair()
	if err != nil {
		return nil, err
	}
	keys := &kbc.OwnKeys{MlKemPrivate: kemPriv, MlKemPublic: kemPub, X25519Private: ecPriv, X25519Public: ecPub}
	fp, err := keys.Fingerprint()
	if err != nil {
		return nil, err
	}
	return &Identity{keys: keys, fp: fp}, nil
}

// Fingerprint returns the out-of-band verification token.
func (id *Identity) Fingerprint() string { return id.fp }

// kdHello is one handshake frame, sent length-prefixed (writeFrame/readFrame from
// secure.go) as JSON. It mirrors the fields KeibiDrop exchanges: public keys, the
// sender's cipher list, the server's chosen suite, and a seed encapsulated to the
// peer for one direction's key.
type kdHello struct {
	Pub     map[string]string `json:"pub,omitempty"`     // base64 x25519 + mlkem public keys
	Ciphers []string          `json:"ciphers,omitempty"` // client's supported suites (frame 1)
	Suite   string            `json:"suite,omitempty"`   // server's chosen suite (frame 2)
	EncX    string            `json:"encx,omitempty"`    // x25519-wrapped 32B seed for sender->peer key
	EncK    string            `json:"enck,omitempty"`    // ml-kem ciphertext for sender->peer key
}

// SecureKD runs KeibiDrop's post-quantum handshake over conn and returns an
// AEAD-encrypted net.Conn. Mutual fingerprint verification; seeds encapsulated to
// the peer's identity keys; a per-direction SEK from HKDF-SHA512.
//
// One bidirectional QUIC stream carries both directions, so unlike KeibiDrop's two
// one-way TCP sockets we derive two keys: Kc2s (client sends) and Ks2c (server
// sends). Each side encapsulates the seed for its OWN send direction, exactly like a
// KeibiDrop outbound handshake, so both KEM operations reuse pkg/crypto unchanged.
// Three frames, no deadlock on a single stream: client writes, reads, writes.
func SecureKD(conn net.Conn, id *Identity, expectedPeerFP string, role Role) (net.Conn, error) {
	switch role {
	case RoleClient:
		return secureKDClient(conn, id, expectedPeerFP)
	case RoleServer:
		return secureKDServer(conn, id, expectedPeerFP)
	default:
		return nil, fmt.Errorf("secure_kd: bad role %d", role)
	}
}

func secureKDClient(conn net.Conn, id *Identity, expectedServerFP string) (net.Conn, error) {
	// Frame 1: our public keys + supported ciphers.
	if err := writeHello(conn, kdHello{Pub: pubMapB64(id.keys), Ciphers: suiteStrings(kbc.SupportedCiphers())}); err != nil {
		return nil, fmt.Errorf("send client hello: %w", err)
	}
	// Frame 2: server's keys, chosen suite, and its encapsulated s2c seed.
	f2, err := readHello(conn)
	if err != nil {
		return nil, fmt.Errorf("read server hello: %w", err)
	}
	serverKeys, err := parsePub(f2.Pub)
	if err != nil {
		return nil, err
	}
	if err := verifyFP(serverKeys, expectedServerFP); err != nil {
		return nil, err
	}
	suite := kbc.CipherSuite(f2.Suite)
	ks2c, err := decapSeed(id.keys, serverKeys, suite, f2.EncX, f2.EncK) // key the server writes with
	if err != nil {
		return nil, fmt.Errorf("derive s2c key: %w", err)
	}
	// Frame 3: our encapsulated c2s seed.
	kc2s, encX, encK, err := encapSeed(id.keys, serverKeys, suite) // key we write with
	if err != nil {
		return nil, err
	}
	if err := writeHello(conn, kdHello{EncX: encX, EncK: encK}); err != nil {
		return nil, fmt.Errorf("send client seed: %w", err)
	}
	return newSecureConnSuite(conn, suite, kc2s, ks2c) // write c2s, read s2c
}

func secureKDServer(conn net.Conn, id *Identity, expectedClientFP string) (net.Conn, error) {
	// Frame 1: client's keys + ciphers.
	f1, err := readHello(conn)
	if err != nil {
		return nil, fmt.Errorf("read client hello: %w", err)
	}
	clientKeys, err := parsePub(f1.Pub)
	if err != nil {
		return nil, err
	}
	if err := verifyFP(clientKeys, expectedClientFP); err != nil {
		return nil, err
	}
	suite := kbc.NegotiateCipher(kbc.SupportedCiphers(), parseSuites(f1.Ciphers))
	// Frame 2: our keys, the negotiated suite, and our encapsulated s2c seed.
	ks2c, encX, encK, err := encapSeed(id.keys, clientKeys, suite) // key we write with
	if err != nil {
		return nil, err
	}
	if err := writeHello(conn, kdHello{Pub: pubMapB64(id.keys), Suite: string(suite), EncX: encX, EncK: encK}); err != nil {
		return nil, fmt.Errorf("send server hello: %w", err)
	}
	// Frame 3: client's encapsulated c2s seed.
	f3, err := readHello(conn)
	if err != nil {
		return nil, fmt.Errorf("read client seed: %w", err)
	}
	kc2s, err := decapSeed(id.keys, clientKeys, suite, f3.EncX, f3.EncK) // key the client writes with
	if err != nil {
		return nil, fmt.Errorf("derive c2s key: %w", err)
	}
	return newSecureConnSuite(conn, suite, ks2c, kc2s) // write s2c, read c2s
}

// encapSeed picks a random 32-byte seed, wraps it to the peer's X25519 key and
// ML-KEM-encapsulates a second secret to the peer's ML-KEM key, then derives the
// directional SEK exactly like KeibiDrop's outbound handshake.
func encapSeed(own *kbc.OwnKeys, peer *kbc.PeerKeys, suite kbc.CipherSuite) (key []byte, encXB64, encKB64 string, err error) {
	seed1 := kbc.GenerateSeed()
	encX, err := kbc.X25519Encapsulate(seed1, own.X25519Private, peer.X25519Public)
	if err != nil {
		return nil, "", "", fmt.Errorf("x25519 encapsulate: %w", err)
	}
	seed2, encK := peer.MlKemPublic.Encapsulate()
	key, err = kbc.DeriveKey(suite, seed1, seed2)
	if err != nil {
		return nil, "", "", fmt.Errorf("derive key: %w", err)
	}
	return key, b64(encX), b64(encK), nil
}

// decapSeed reverses encapSeed with our private keys, yielding the same SEK.
func decapSeed(own *kbc.OwnKeys, peer *kbc.PeerKeys, suite kbc.CipherSuite, encXB64, encKB64 string) ([]byte, error) {
	encX, err := unb64(encXB64)
	if err != nil {
		return nil, fmt.Errorf("decode encx: %w", err)
	}
	encK, err := unb64(encKB64)
	if err != nil {
		return nil, fmt.Errorf("decode enck: %w", err)
	}
	seed1, err := kbc.X25519Decapsulate(encX, own.X25519Private, peer.X25519Public)
	if err != nil {
		return nil, fmt.Errorf("x25519 decapsulate: %w", err)
	}
	seed2, err := own.MlKemPrivate.Decapsulate(encK)
	if err != nil {
		return nil, fmt.Errorf("mlkem decapsulate: %w", err)
	}
	return kbc.DeriveKey(suite, seed1, seed2)
}

// newSecureConnSuite builds the record layer (secureConn from secure.go) with AEADs
// for the negotiated suite, so this path honours KeibiDrop's cipher negotiation
// (AES-256-GCM on hardware-AES machines, ChaCha20-Poly1305 otherwise).
func newSecureConnSuite(inner net.Conn, suite kbc.CipherSuite, wkey, rkey []byte) (net.Conn, error) {
	waead, err := kbc.NewAEAD(suite, wkey)
	if err != nil {
		return nil, fmt.Errorf("write aead: %w", err)
	}
	raead, err := kbc.NewAEAD(suite, rkey)
	if err != nil {
		return nil, fmt.Errorf("read aead: %w", err)
	}
	return &secureConn{Conn: inner, waead: waead, raead: raead}, nil
}

func verifyFP(peer *kbc.PeerKeys, expected string) error {
	got, err := peer.Fingerprint()
	if err != nil {
		return err
	}
	if expected == "TOFU" {
		return nil
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
		return fmt.Errorf("fingerprint mismatch: got %s want %s", got, expected)
	}
	return nil
}

func pubMapB64(k *kbc.OwnKeys) map[string]string {
	return map[string]string{
		"x25519": b64(k.X25519Public.Bytes()),
		"mlkem":  b64(k.MlKemPublic.Bytes()),
	}
}

func parsePub(m map[string]string) (*kbc.PeerKeys, error) {
	x, err := unb64(m["x25519"])
	if err != nil {
		return nil, fmt.Errorf("decode x25519: %w", err)
	}
	k, err := unb64(m["mlkem"])
	if err != nil {
		return nil, fmt.Errorf("decode mlkem: %w", err)
	}
	return kbc.ParsePeerKeys(map[string][]byte{"x25519": x, "mlkem": k})
}

func suiteStrings(s []kbc.CipherSuite) []string {
	out := make([]string, len(s))
	for i, c := range s {
		out[i] = string(c)
	}
	return out
}

func parseSuites(s []string) []kbc.CipherSuite {
	if len(s) == 0 {
		return []kbc.CipherSuite{kbc.CipherChaCha20}
	}
	out := make([]kbc.CipherSuite, len(s))
	for i, c := range s {
		out[i] = kbc.CipherSuite(c)
	}
	return out
}

func b64(b []byte) string        { return base64.StdEncoding.EncodeToString(b) }
func unb64(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }

func writeHello(conn net.Conn, h kdHello) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	return writeFrame(conn, b)
}

func readHello(conn net.Conn) (kdHello, error) {
	b, err := readFrame(conn)
	if err != nil {
		return kdHello{}, err
	}
	var h kdHello
	if err := json.Unmarshal(b, &h); err != nil {
		return kdHello{}, fmt.Errorf("bad hello json: %w", err)
	}
	return h, nil
}

// ---- gRPC wiring: gRPC over KeibiDrop's PQC handshake over QUIC ----

type secureKDListener struct {
	net.Listener
	id     *Identity
	peerFP string
}

func (l secureKDListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		sc, err := SecureKD(c, l.id, l.peerFP, RoleServer)
		if err != nil {
			_ = c.Close()
			continue // a failed handshake (e.g. wrong fingerprint) drops that conn
		}
		return sc, nil
	}
}

// ServeGRPCKD serves svc over gRPC over KeibiDrop's PQC handshake over QUIC. The
// server authenticates each client against expectedClientFP.
func ServeGRPCKD(addr string, svc pb.BenchServiceServer, id *Identity, expectedClientFP string, opts ...grpc.ServerOption) (*grpc.Server, net.Addr, error) {
	return ServeGRPCKDOver(QUIC(), addr, svc, id, expectedClientFP, opts...)
}

// ServeGRPCKDOver is ServeGRPCKD over an explicit transport. The PQC handshake is
// transport-agnostic (SecureKD wraps any net.Conn), so the same authenticated,
// post-quantum-secured gRPC runs over TCP (the bulk channel) or QUIC (interactive).
func ServeGRPCKDOver(tr Transport, addr string, svc pb.BenchServiceServer, id *Identity, expectedClientFP string, opts ...grpc.ServerOption) (*grpc.Server, net.Addr, error) {
	ln, err := tr.Listen(addr)
	if err != nil {
		return nil, nil, err
	}
	sln := secureKDListener{Listener: ln, id: id, peerFP: expectedClientFP}
	all := append([]grpc.ServerOption{
		grpc.MaxRecvMsgSize(maxFrame),
		grpc.MaxSendMsgSize(maxFrame),
	}, opts...)
	s := grpc.NewServer(all...)
	cpb.RegisterControlServer(s, controlService{}) // heartbeat responder, always present
	pb.RegisterBenchServiceServer(s, svc)
	go func() { _ = s.Serve(sln) }()
	return s, sln.Addr(), nil
}

// DialGRPCKD dials svc over QUIC, authenticating the server against expectedServerFP.
func DialGRPCKD(addr string, id *Identity, expectedServerFP string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	return DialGRPCKDOver(QUIC(), addr, id, expectedServerFP, opts...)
}

// DialGRPCKDOver is DialGRPCKD over an explicit transport (QUIC for interactive, TCP
// for bulk). Same PQC handshake either way, so both channels are equally secured with
// one identity + fingerprint.
func DialGRPCKDOver(tr Transport, addr string, id *Identity, expectedServerFP string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		c, err := tr.Dial(ctx, addr)
		if err != nil {
			return nil, err
		}
		sc, err := SecureKD(c, id, expectedServerFP, RoleClient)
		if err != nil {
			_ = c.Close()
			return nil, err
		}
		return sc, nil
	}
	all := append([]grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxFrame), grpc.MaxCallSendMsgSize(maxFrame)),
	}, opts...)
	return grpc.NewClient("passthrough:///"+addr, all...)
}
