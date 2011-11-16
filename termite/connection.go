package termite

import (
	"bytes"
	"crypto/hmac"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const _SOCKET = ".termite-socket"

const challengeLength = 20

var Hostname string

func init() {
	var err error
	Hostname, err = os.Hostname()
	if err != nil {
		log.Println("hostname", err)
	}
}

func sign(conn net.Conn, challenge []byte, secret []byte, local bool) []byte {
	h := hmac.NewSHA1(secret)
	h.Write(challenge)
	l := conn.LocalAddr()
	r := conn.RemoteAddr()
	connSignature := ""
	if local {
		connSignature = fmt.Sprintf("%v-%v", l, r)
	} else {
		connSignature = fmt.Sprintf("%v-%v", r, l)
	}
	h.Write([]byte(connSignature))
	return h.Sum()
}

// Symmetrical authentication using HMAC-SHA1.
//
// To authenticate, we do  the following:
//
// * Receive 20-byte random challenge
// * Using the secret, sign (challenge + remote address + local address)
// * Return the signature
//
// TODO - should probably use SSL/TLS? Figure out what is useful and
// necessary here.
func Authenticate(conn net.Conn, secret []byte) error {
	challenge := RandomBytes(challengeLength)

	_, err := conn.Write(challenge)
	if err != nil {
		return err
	}
	expected := sign(conn, challenge, secret, true)

	remoteChallenge := make([]byte, challengeLength)
	n, err := conn.Read(remoteChallenge)
	if err != nil {
		return err
	}
	remoteChallenge = remoteChallenge[:n]
	_, err = conn.Write(sign(conn, remoteChallenge, secret, false))

	response := make([]byte, len(expected))
	n, err = conn.Read(response)
	if err != nil {
		return err
	}
	response = response[:n]

	if bytes.Compare(response, expected) != 0 {
		log.Println("Authentication failure from", conn.RemoteAddr())
		conn.Close()
		return errors.New("Mismatch in response")
	}

	expectAck := []byte("OK")
	conn.Write(expectAck)

	ack := make([]byte, len(expectAck))
	n, err = conn.Read(ack)
	if err != nil {
		return err
	}

	ack = ack[:n]
	if bytes.Compare(expectAck, ack) != 0 {
		fmt.Println(expectAck, ack)
		return errors.New("Missing ack reply")
	}

	return nil
}

type Listener struct {
	net.Listener
	secret []byte
}

func AuthenticatedListener(port int, secret []byte) net.Listener {
	host, _ := os.Hostname()
	_ = host
	addr := fmt.Sprintf(":%d", port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal("net.Listen", err)
	}
	log.Println("Listening to", listener.Addr())
	return &Listener{listener, secret}
}

func (me *Listener) Accept() (net.Conn, error) {
	for {
		c, err := me.Listener.Accept()
		if err != nil {
			return nil, err
		}
		err = Authenticate(c, me.secret)
		if err != nil {
			c.Close()
			continue
		}
		return c, nil
	}
	return nil, io.EOF
}

// ids:
//
const (
	RPC_CHANNEL = "rpc....."
	_ID_FMT     = "id%06d"
	HEADER_LEN  = 8
)

func ConnectionId() string {
	id := rand.Intn(1e6)
	return fmt.Sprintf(_ID_FMT, id)
}

type pendingConnection struct {
	Id    string
	Ready sync.Cond
	Conn  net.Conn
}

// PendingConnections manages a list of connections, indexed by ID.
// The id is sent as the first 8 bytes, after the authentication.
type PendingConnections struct {
	connectionsMutex sync.Mutex
	connections      map[string]*pendingConnection
}

func NewPendingConnections() *PendingConnections {
	return &PendingConnections{
		connections: make(map[string]*pendingConnection),
	}
}

func (me *PendingConnections) newPendingConnection(id string) *pendingConnection {
	p := &pendingConnection{
		Id: id,
	}
	p.Ready.L = &me.connectionsMutex
	return p
}

func (me *PendingConnections) WaitConnection(id string) net.Conn {
	me.connectionsMutex.Lock()
	defer me.connectionsMutex.Unlock()
	p := me.connections[id]
	if p == nil {
		p = me.newPendingConnection(id)
		me.connections[id] = p
	}

	for p.Conn == nil {
		p.Ready.Wait()
	}

	me.connections[id] = nil
	return p.Conn
}

// Returns false if caller should handle the connection.
func (me *PendingConnections) Accept(conn net.Conn) bool {
	idBytes := make([]byte, HEADER_LEN)
	n, err := conn.Read(idBytes)
	if n != HEADER_LEN || err != nil {
		conn.Close()
		return true
	}
	id := string(idBytes)
	if id == RPC_CHANNEL {
		return false
	}

	me.connectionsMutex.Lock()
	defer me.connectionsMutex.Unlock()
	p := me.connections[id]
	if p == nil {
		p = me.newPendingConnection(id)
		me.connections[id] = p
	}
	if p.Conn != nil {
		log.Panicf("accepted the same connection id twice: %s", id)
	}
	p.Conn = conn
	p.Ready.Signal()
	return true
}

func DialTypedConnection(addr string, id string, secret []byte) (net.Conn, error) {
	if len(id) != HEADER_LEN {
		log.Fatal("id != 8", id, len(id))
	}
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}

	err = Authenticate(conn, secret)
	if err != nil {
		return nil, err
	}
	_, err = io.WriteString(conn, id)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func OpenSocketConnection(socket string, channel string, timeout int64) net.Conn {
	delay := int64(0)
	conn, err := net.Dial("unix", socket)
	for try := 0; err != nil && delay < timeout; try++ {
		delay = int64(1.5+0.5*rand.Float64()*float64(delay)) + 0.02e9
		time.Sleep(int64(delay))
		conn, err = net.Dial("unix", socket)
		continue
	}
	if err != nil {
		log.Fatal("OpenSocketConnection: ", err)
	}

	if len(channel) != HEADER_LEN {
		panic(channel)
	}
	_, err = io.WriteString(conn, channel)
	if err != nil {
		log.Fatal("WriteString", err)
	}
	return conn
}

func FindSocket() string {
	socket := os.Getenv("TERMITE_SOCKET")
	wd, _ := os.Getwd()
	if socket == "" {
		dir := wd
		for dir != "" && dir != "/" {
			cand := filepath.Join(dir, _SOCKET)
			fi, _ := os.Lstat(cand)
			if fi != nil && fi.IsSocket() {
				socket = cand
				break
			}
			dir = filepath.Clean(filepath.Join(dir, ".."))
		}
	}
	return socket
}
