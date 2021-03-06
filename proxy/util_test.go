package proxy

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"github.com/armon/go-socks5"
	"github.com/elazarl/goproxy"
	"github.com/elazarl/goproxy/ext/auth"
	"github.com/grepplabs/kafka-proxy/config"
	"github.com/pkg/errors"
	"io/ioutil"
	"math/big"
	"net"
	"net/http"
	"os"
	"time"
)

func makeTLSPipe(conf *config.Config) (c1, c2 net.Conn, stop func(), err error) {
	rawDialer := directDialer{
		dialTimeout: 3 * time.Second,
		keepAlive:   60 * time.Second,
	}

	clientConfig, err := newTLSClientConfig(conf)
	if err != nil {
		return nil, nil, nil, err
	}
	tlsDialer := tlsDialer{
		timeout:   3 * time.Second,
		rawDialer: rawDialer,
		config:    clientConfig,
	}
	serverConfig, err := newTLSListenerConfig(conf)
	if err != nil {
		return nil, nil, nil, err
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverConfig)
	if err != nil {
		return nil, nil, nil, err
	}
	// Start a connection between two endpoints.
	var err1, err2 error
	done := make(chan bool)
	go func() {
		c2, err2 = ln.Accept()
		close(done)
		// will force handshake completion
		buf := make([]byte, 0)
		c2.Read(buf)

		tlscon, ok := c2.(*tls.Conn)
		if ok {
			state := tlscon.ConnectionState()
			for _, v := range state.PeerCertificates {
				_ = v
				//fmt.Println(x509.MarshalPKIXPublicKey(v.PublicKey))
			}
		}
	}()
	c1, err1 = tlsDialer.Dial(ln.Addr().Network(), ln.Addr().String())
	if err1 != nil {
		ln.Close()
		return nil, nil, nil, err1
	}
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		ln.Close()
		return nil, nil, nil, errors.New("Accept timeout ")
	}

	stop = func() {
		if err1 == nil {
			c1.Close()
		}
		if err2 == nil {
			c2.Close()
		}
		ln.Close()
	}

	switch {
	case err1 != nil:
		stop()
		return nil, nil, nil, err1
	case err2 != nil:
		stop()
		return nil, nil, nil, err2
	default:
		return c1, c2, stop, nil
	}
}

type testCredentials struct {
	username, password string
}

func (s testCredentials) Valid(username, password string) bool {
	return s.username == username && s.password == password
}

func makeTLSSocks5ProxyPipe(conf *config.Config, authenticator socks5.Authenticator, username, password string) (c1, c2 net.Conn, stop func(), err error) {
	socks5Conf := &socks5.Config{}
	if authenticator != nil {
		socks5Conf.AuthMethods = []socks5.Authenticator{authenticator}
	}
	server, err := socks5.New(socks5Conf)
	if err != nil {
		return nil, nil, nil, err
	}
	clientConfig, err := newTLSClientConfig(conf)
	if err != nil {
		return nil, nil, nil, err
	}
	serverConfig, err := newTLSListenerConfig(conf)
	if err != nil {
		return nil, nil, nil, err
	}

	proxy, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, nil, nil, err
	}
	socksDialer := socks5Dialer{
		directDialer: directDialer{
			dialTimeout: 2 * time.Second,
			keepAlive:   60 * time.Second,
		},
		proxyNetwork: proxy.Addr().Network(),
		proxyAddr:    proxy.Addr().String(),
		username:     username,
		password:     password,
	}

	tlsDialer := tlsDialer{
		timeout:   3 * time.Second,
		rawDialer: socksDialer,
		config:    clientConfig,
	}

	target, err := tls.Listen("tcp", "127.0.0.1:0", serverConfig)
	if err != nil {
		proxy.Close()
		return nil, nil, nil, err
	}
	// Start a connection between two endpoints.
	var err0, err1, err2 error
	var c0 net.Conn
	go func() {
		c0, err0 = proxy.Accept()
		if err0 == nil {
			err0 = server.ServeConn(c0)
		}
	}()
	done := make(chan bool)
	go func() {
		c2, err2 = target.Accept()
		close(done)
		if err2 != nil {
			return
		}
		// will force handshake completion
		buf := make([]byte, 0)
		c2.Read(buf)

		tlscon, ok := c2.(*tls.Conn)
		if ok {
			state := tlscon.ConnectionState()
			for _, v := range state.PeerCertificates {
				_ = v
				//fmt.Println(x509.MarshalPKIXPublicKey(v.PublicKey))
			}
		}
	}()
	stop = func() {
		if err1 == nil {
			c1.Close()
		}
		if err2 == nil {
			c2.Close()
		}
		target.Close()
		proxy.Close()
	}

	c1, err1 = tlsDialer.Dial(target.Addr().Network(), target.Addr().String())
	if err1 != nil {
		target.Close()
		return nil, nil, nil, err1
	}
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		target.Close()
		return nil, nil, nil, errors.New("Accept timeout ")
	}

	switch {
	case err1 != nil:
		stop()
		return nil, nil, nil, err1
	case err2 != nil:
		stop()
		return nil, nil, nil, err2
	default:
		return c1, c2, stop, nil
	}
}

func makeTLSHttpProxyPipe(conf *config.Config, proxyusername, proxypassword string, username, password string) (c1, c2 net.Conn, stop func(), err error) {
	server := goproxy.NewProxyHttpServer()

	if proxyusername != "" && proxypassword != "" {
		server.OnRequest().HandleConnect(auth.BasicConnect("", func(user, passwd string) bool {
			return user == proxyusername && passwd == proxypassword
		}))
	}

	if err != nil {
		return nil, nil, nil, err
	}
	clientConfig, err := newTLSClientConfig(conf)
	if err != nil {
		return nil, nil, nil, err
	}
	serverConfig, err := newTLSListenerConfig(conf)
	if err != nil {
		return nil, nil, nil, err
	}

	proxy, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, nil, nil, err
	}
	httpProxy := &httpProxy{
		forwardDialer: directDialer{
			dialTimeout: 2 * time.Second,
			keepAlive:   60 * time.Second,
		},
		network:  proxy.Addr().Network(),
		hostPort: proxy.Addr().String(),
		username: username,
		password: password,
	}

	tlsDialer := tlsDialer{
		timeout:   3 * time.Second,
		rawDialer: httpProxy,
		config:    clientConfig,
	}

	target, err := tls.Listen("tcp", "127.0.0.1:0", serverConfig)
	if err != nil {
		proxy.Close()
		return nil, nil, nil, err
	}
	// Start a connection between two endpoints.
	var err1, err2 error
	go func() {
		http.Serve(proxy, server)
	}()
	done := make(chan bool)
	go func() {
		c2, err2 = target.Accept()
		close(done)
		if err2 != nil {
			return
		}
		// will force handshake completion
		buf := make([]byte, 0)
		c2.Read(buf)

		tlscon, ok := c2.(*tls.Conn)
		if ok {
			state := tlscon.ConnectionState()
			for _, v := range state.PeerCertificates {
				_ = v
				//fmt.Println(x509.MarshalPKIXPublicKey(v.PublicKey))
			}
		}
	}()
	stop = func() {
		if err1 == nil {
			c1.Close()
		}
		if err2 == nil {
			c2.Close()
		}
		target.Close()
		proxy.Close()
	}

	c1, err1 = tlsDialer.Dial(target.Addr().Network(), target.Addr().String())
	if err1 != nil {
		target.Close()
		return nil, nil, nil, err1
	}
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		target.Close()
		return nil, nil, nil, errors.New("Accept timeout ")
	}

	switch {
	case err1 != nil:
		stop()
		return nil, nil, nil, err1
	case err2 != nil:
		stop()
		return nil, nil, nil, err2
	default:
		return c1, c2, stop, nil
	}
}

func makePipe() (c1, c2 net.Conn, stop func(), err error) {
	dialer := directDialer{
		dialTimeout: 2 * time.Second,
		keepAlive:   60 * time.Second,
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, nil, nil, err
	}

	// Start a connection between two endpoints.
	var err1, err2 error
	done := make(chan bool)
	go func() {
		c2, err2 = ln.Accept()
		close(done)
	}()

	c1, err1 = dialer.Dial(ln.Addr().Network(), ln.Addr().String())
	stop = func() {
		if err1 == nil {
			c1.Close()
		}
		if err2 == nil {
			c2.Close()
		}
		ln.Close()
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		stop()
		return nil, nil, nil, errors.New("Accept timeout")
	}

	switch {
	case err1 != nil:
		stop()
		return nil, nil, nil, err1
	case err2 != nil:
		stop()
		return nil, nil, nil, err2
	default:
		return c1, c2, stop, nil
	}
}

func makeSocks5ProxyPipe() (c1, c2 net.Conn, stop func(), err error) {
	server, err := socks5.New(&socks5.Config{})
	if err != nil {
		return nil, nil, nil, err
	}
	proxy, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, nil, nil, err
	}
	target, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		proxy.Close()
		return nil, nil, nil, err
	}
	socksDialer := socks5Dialer{
		directDialer: directDialer{
			dialTimeout: 2 * time.Second,
			keepAlive:   60 * time.Second,
		},
		proxyNetwork: proxy.Addr().Network(),
		proxyAddr:    proxy.Addr().String(),
	}

	var err0, err1, err2 error
	var c0 net.Conn
	go func() {
		c0, err0 = proxy.Accept()
		if err0 == nil {
			err0 = server.ServeConn(c0)
		}
	}()

	done := make(chan bool)
	go func() {
		c2, err2 = target.Accept()
		close(done)
	}()

	c1, err1 = socksDialer.Dial(target.Addr().Network(), target.Addr().String())

	stop = func() {
		if err1 == nil {
			c1.Close()
		}
		if err2 == nil {
			c2.Close()
		}
		target.Close()
		proxy.Close()
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		stop()
		return nil, nil, nil, errors.New("Accept timeout")
	}

	switch {
	case err0 != nil:
		stop()
		return nil, nil, nil, err0
	case err1 != nil:
		stop()
		return nil, nil, nil, err1
	case err2 != nil:
		stop()
		return nil, nil, nil, err2
	default:
		return c1, c2, stop, nil
	}
}

func makeHttpProxyPipe() (c1, c2 net.Conn, stop func(), err error) {
	server := goproxy.NewProxyHttpServer()
	//server.Verbose = true

	if err != nil {
		return nil, nil, nil, err
	}
	proxy, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, nil, nil, err
	}
	target, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		proxy.Close()
		return nil, nil, nil, err
	}
	httpProxy := httpProxy{
		forwardDialer: directDialer{
			dialTimeout: 2 * time.Second,
			keepAlive:   60 * time.Second,
		},
		network:  proxy.Addr().Network(),
		hostPort: proxy.Addr().String(),
	}

	var err0, err1, err2 error
	go func() {
		err0 = http.Serve(proxy, server)
	}()

	done := make(chan bool)
	go func() {
		c2, err2 = target.Accept()
		close(done)
	}()

	c1, err1 = httpProxy.Dial(target.Addr().Network(), target.Addr().String())

	stop = func() {
		if err1 == nil {
			c1.Close()
		}
		if err2 == nil {
			c2.Close()
		}
		target.Close()
		proxy.Close()
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		stop()
		return nil, nil, nil, errors.New("Accept timeout")
	}

	switch {
	case err0 != nil:
		stop()
		return nil, nil, nil, err0
	case err1 != nil:
		stop()
		return nil, nil, nil, err1
	case err2 != nil:
		stop()
		return nil, nil, nil, err2
	default:
		return c1, c2, stop, nil
	}
}

func generateCert(catls *tls.Certificate, certFile *os.File, keyFile *os.File) error {
	// Prepare certificate
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization:  []string{"ORGANIZATION_NAME"},
			Country:       []string{"COUNTRY_CODE"},
			Province:      []string{"PROVINCE"},
			Locality:      []string{"CITY"},
			StreetAddress: []string{"ADDRESS"},
			PostalCode:    []string{"POSTAL_CODE"},
			CommonName:    "localhost",
		},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		SubjectKeyId: []byte{1, 2, 3, 4, 6},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IP([]byte{127, 0, 0, 1})},
	}
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	pub := &priv.PublicKey

	// tls cert -> x509 cert
	ca, err := x509.ParseCertificate(catls.Certificate[0])
	if err != nil {
		return err
	}

	// Sign the certificate
	cert_b, err := x509.CreateCertificate(rand.Reader, cert, ca, pub, catls.PrivateKey)
	if err != nil {
		return err
	}
	// Public key
	err = pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: cert_b})
	if err != nil {
		return err
	}
	err = certFile.Sync()
	if err != nil {
		return err
	}
	// Private key
	err = pem.Encode(keyFile, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	err = keyFile.Sync()
	if err != nil {
		return err
	}
	return nil
}

func generateCA(certFile *os.File, keyFile *os.File) (*tls.Certificate, error) {
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1653),
		Subject: pkix.Name{
			Organization:  []string{"ORGANIZATION_NAME"},
			Country:       []string{"COUNTRY_CODE"},
			Province:      []string{"PROVINCE"},
			Locality:      []string{"CITY"},
			StreetAddress: []string{"ADDRESS"},
			PostalCode:    []string{"POSTAL_CODE"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}

	pub := &priv.PublicKey
	ca_b, err := x509.CreateCertificate(rand.Reader, ca, ca, pub, priv)
	if err != nil {
		return nil, err
	}

	// Public key
	err = pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: ca_b})
	if err != nil {
		return nil, err
	}
	err = certFile.Sync()
	if err != nil {
		return nil, err
	}
	// Private key
	err = pem.Encode(keyFile, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	if err != nil {
		return nil, err
	}
	err = keyFile.Sync()
	if err != nil {
		return nil, err
	}
	// Load CA
	catls, err := tls.LoadX509KeyPair(certFile.Name(), keyFile.Name())
	if err != nil {
		return nil, err
	}

	ca, err = x509.ParseCertificate(catls.Certificate[0])
	if err != nil {
		return nil, err
	}
	return &catls, nil
}

func NewCertsBundle() *CertsBundle {
	bundle := &CertsBundle{}
	dirName, err := ioutil.TempDir("", "tls-test")
	if err != nil {
		panic(err)
	}
	bundle.CACert, err = ioutil.TempFile(dirName, "ca-cert-")
	if err != nil {
		panic(err)
	}
	bundle.CAKey, err = ioutil.TempFile(dirName, "ca-key-")
	if err != nil {
		panic(err)
	}
	bundle.ServerCert, err = ioutil.TempFile(dirName, "server-cert-")
	if err != nil {
		panic(err)
	}
	bundle.ServerKey, err = ioutil.TempFile(dirName, "server-key-")
	if err != nil {
		panic(err)
	}
	bundle.ClientCert, err = ioutil.TempFile(dirName, "client-cert-")
	if err != nil {
		panic(err)
	}
	bundle.ClientKey, err = ioutil.TempFile("", "client-key-")
	if err != nil {
		panic(err)
	}
	// generate certs
	catls, err := generateCA(bundle.CACert, bundle.CAKey)
	if err != nil {
		panic(err)
	}
	err = generateCert(catls, bundle.ServerCert, bundle.ServerKey)
	if err != nil {
		panic(err)
	}
	err = generateCert(catls, bundle.ClientCert, bundle.ClientKey)
	if err != nil {
		panic(err)
	}
	return bundle
}

func (bundle *CertsBundle) Close() {
	os.Remove(bundle.CACert.Name())
	os.Remove(bundle.CAKey.Name())
	os.Remove(bundle.ServerCert.Name())
	os.Remove(bundle.ServerKey.Name())
	os.Remove(bundle.ClientCert.Name())
	os.Remove(bundle.ClientKey.Name())
	os.Remove(bundle.dirName)
}
