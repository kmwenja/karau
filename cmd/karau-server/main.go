package main

import (
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"strings"

	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"
)

func redirectConnRequests(reqs <-chan *ssh.Request, c ssh.Conn, errChan chan error) {
	for req := range reqs {
		success, resp, err := c.SendRequest(req.Type, req.WantReply, req.Payload)
		if err != nil {
			errChan <- errors.Wrap(err, "redirect conn requests: could not send request")
			return
		}
		if req.WantReply {
			err := req.Reply(success, resp)
			if err != nil {
				errChan <- errors.Wrap(err, "redirect conn requests: could not reply to request")
				return
			}
		}
	}
}

func redirectChanRequests(reqs <-chan *ssh.Request, aChan ssh.Channel, errChan chan error) {
	for req := range reqs {
		success, err := aChan.SendRequest(req.Type, req.WantReply, req.Payload)
		if err != nil {
			errChan <- errors.Wrap(err, "redirect chan requests: could not send request")
			return
		}
		if req.WantReply {
			var empty []byte
			err := req.Reply(success, empty)
			if err != nil {
				errChan <- errors.Wrap(err, "redirect chan requests: could not reply to request")
				return
			}
		}
	}
}

func redirectChans(chans <-chan ssh.NewChannel, conn ssh.Conn, errChan chan error) {
	for newChan := range chans {
		connChan, connChanReqs, err := conn.OpenChannel(
			newChan.ChannelType(), newChan.ExtraData())

		if err != nil {
			if oce, ok := err.(*ssh.OpenChannelError); ok {
				err := newChan.Reject(oce.Reason, oce.Message)
				if err != nil {
					errChan <- errors.Wrap(err, "could not reject channel open")
					return
				}
			} else {
				errChan <- errors.Wrap(err, "could not open downstream channel")
				return
			}
		}

		acceptedChan, acceptedChanReqs, err := newChan.Accept()
		if err != nil {
			errChan <- errors.Wrap(err, "could not accept channel")
			return
		}

		go redirectChanRequests(acceptedChanReqs, connChan, errChan)
		go redirectChanRequests(connChanReqs, acceptedChan, errChan)

		go func() {
			_, err := io.Copy(acceptedChan, connChan)
			if err != nil {
				errChan <- errors.Wrap(err, "could not write to accepted chan")
			}
		}()

		_, err = io.Copy(connChan, acceptedChan)
		if err != nil {
			errChan <- errors.Wrap(err, "could not write to conn chan")
			return
		}
	}
}

func main() {
	var (
		password       = flag.String("password", "world", "master password")
		hostKey        = flag.String("host-key", "$HOME/.ssh/id_rsa", "host key")
		hostPassphrase = flag.String("host-passphrase", "", "host passphrase")
	)
	flag.Parse()

	// start an ssh server and print out the remote addr and user
	config := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			log.Printf("Conn Meta: User(%s) RemoteAddr(%s)\n", c.User(), c.LocalAddr())
			if string(pass) != *password {
				return nil, fmt.Errorf("password rejected for %q", c.User())
			}

			parts := strings.Split(c.User(), "--")
			if len(parts) < 4 {
				return nil, fmt.Errorf("invalid username '%q'", c.User())
			}

			return &ssh.Permissions{
				Extensions: map[string]string{
					"karau-target-user":     parts[0],
					"karau-target-password": parts[1],
					"karau-target-host":     parts[2],
					"karau-target-port":     parts[3],
				},
			}, nil
		},
	}

	privateBytes, err := ioutil.ReadFile(*hostKey)
	if err != nil {
		log.Fatal("Failed to load private key:", err)
	}

	private, err := ssh.ParsePrivateKeyWithPassphrase(privateBytes, []byte(*hostPassphrase))
	if err != nil {
		log.Fatal("Failed to parse private key:", err)
	}

	config.AddHostKey(private)

	listener, err := net.Listen("tcp", "0.0.0.0:2022")
	if err != nil {
		log.Fatal("Failed to listen for connection:", err)
	}

	nConn, err := listener.Accept()
	if err != nil {
		log.Fatal("Failed to accept incoming connection:", err)
	}

	serverConn, serverChans, serverReqs, err := ssh.NewServerConn(nConn, config)
	if err != nil {
		log.Fatal("Failed to handshake:", err)
	}

	clientConfig := &ssh.ClientConfig{
		User: serverConn.Permissions.Extensions["karau-target-user"],
		Auth: []ssh.AuthMethod{
			ssh.Password(serverConn.Permissions.Extensions["karau-target-password"]),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	addr := fmt.Sprintf(
		"%s:%s", serverConn.Permissions.Extensions["karau-target-host"],
		serverConn.Permissions.Extensions["karau-target-port"])
	cConn, err := net.Dial("tcp", addr)
	if err != nil {
		log.Fatal("Could not dial downstream:", err)
	}

	clientConn, clientChans, clientReqs, err := ssh.NewClientConn(
		cConn, addr, clientConfig)
	if err != nil {
		log.Fatal("Could not ssh connect downstream:", err)
	}

	var errChan chan error

	// pass through original client's requests to server's client
	go redirectConnRequests(serverReqs, clientConn, errChan)

	// pass through server's client's requests to original client
	go redirectConnRequests(clientReqs, serverConn, errChan)

	// pass through original client's channels to server's client
	go redirectChans(serverChans, clientConn, errChan)

	// pass through server's client's channels to original client
	go redirectChans(clientChans, serverConn, errChan)

	err = <-errChan
	log.Fatal(err)
}
