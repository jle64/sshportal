package bastion // import "moul.io/sshportal/pkg/bastion"

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/gliderlabs/ssh"
	"github.com/sabban/bastion/pkg/logchannel"
	gossh "golang.org/x/crypto/ssh"
)

type sessionConfig struct {
	Addr         string
	Logs         string
	ClientConfig *gossh.ClientConfig
}

func multiChannelHandler(conn *gossh.ServerConn, newChan gossh.NewChannel, ctx ssh.Context, configs []sessionConfig, sessionID uint) error {
	var lastClient *gossh.Client
	switch newChan.ChannelType() {
	case "session":
		lch, lreqs, err := newChan.Accept()
		// TODO: defer clean closer
		if err != nil {
			// TODO: trigger event callback
			return nil
		}

		// go through all the hops
		for _, config := range configs {
			var client *gossh.Client
			if lastClient == nil {
				client, err = gossh.Dial("tcp", config.Addr, config.ClientConfig)
			} else {
				rconn, err := lastClient.Dial("tcp", config.Addr)
				if err != nil {
					return err
				}
				ncc, chans, reqs, err := gossh.NewClientConn(rconn, config.Addr, config.ClientConfig)
				if err != nil {
					return err
				}
				client = gossh.NewClient(ncc, chans, reqs)
			}
			if err != nil {
				lch.Close() // fix #56
				return err
			}
			defer func() { _ = client.Close() }()
			lastClient = client
		}

		rch, rreqs, err := lastClient.OpenChannel("session", []byte{})
		if err != nil {
			return err
		}
		user := conn.User()
		actx := ctx.Value(authContextKey).(*authContext)
		username := actx.user.Name
		// pipe everything
		return pipe(lreqs, rreqs, lch, rch, configs[len(configs)-1].Logs, user, username, sessionID, newChan)
	case "direct-tcpip":
		lch, lreqs, err := newChan.Accept()
		// TODO: defer clean closer
		if err != nil {
			// TODO: trigger event callback
			return nil
		}

		// go through all the hops
		for _, config := range configs {
			var client *gossh.Client
			if lastClient == nil {
				client, err = gossh.Dial("tcp", config.Addr, config.ClientConfig)
			} else {
				rconn, err := lastClient.Dial("tcp", config.Addr)
				if err != nil {
					return err
				}
				ncc, chans, reqs, err := gossh.NewClientConn(rconn, config.Addr, config.ClientConfig)
				if err != nil {
					return err
				}
				client = gossh.NewClient(ncc, chans, reqs)
			}
			if err != nil {
				lch.Close()
				return err
			}
			defer func() { _ = client.Close() }()
			lastClient = client
		}

		d := logTunnelForwardData{}
		if err := gossh.Unmarshal(newChan.ExtraData(), &d); err != nil {
			return err
		}
		rch, rreqs, err := lastClient.OpenChannel("direct-tcpip", newChan.ExtraData())
		if err != nil {
			return err
		}
		user := conn.User()
		actx := ctx.Value(authContextKey).(*authContext)
		username := actx.user.Name
		// pipe everything
		return pipe(lreqs, rreqs, lch, rch, configs[len(configs)-1].Logs, user, username, sessionID, newChan)
	default:
		if err := newChan.Reject(gossh.UnknownChannelType, "unsupported channel type"); err != nil {
			log.Printf("failed to reject chan: %v", err)
		}
		return nil
	}
}

func pipe(lreqs, rreqs <-chan *gossh.Request, lch, rch gossh.Channel, logsLocation string, user string, username string, sessionID uint, newChan gossh.NewChannel) error {
	defer func() {
		_ = lch.Close()
		_ = rch.Close()
	}()

	errch := make(chan error, 1)
	quit := make(chan string, 1)
	channeltype := newChan.ChannelType()

	filename := strings.Join([]string{logsLocation, "/", user, "-", username, "-", channeltype, "-", fmt.Sprint(sessionID), "-", time.Now().Format(time.RFC3339)}, "") // get user
	f, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0440)
	defer func() {
		_ = f.Close()
	}()

	if err != nil {
		log.Fatalf("error: %v", err)
	}

	log.Printf("Session %v is recorded in %v", channeltype, filename)
	if channeltype == "session" {
		wrappedlch := logchannel.New(lch, f)
		go func(quit chan string) {
			_, _ = io.Copy(wrappedlch, rch)
			quit <- "rch"
		}(quit)

		go func(quit chan string) {
			_, _ = io.Copy(rch, lch)
			quit <- "lch"
		}(quit)
	}
	if channeltype == "direct-tcpip" {
		d := logTunnelForwardData{}
		if err := gossh.Unmarshal(newChan.ExtraData(), &d); err != nil {
			return err
		}
		wrappedlch := newLogTunnel(lch, f, d.SourceHost)
		wrappedrch := newLogTunnel(rch, f, d.DestinationHost)
		go func(quit chan string) {
			_, _ = io.Copy(wrappedlch, rch)
			quit <- "rch"
		}(quit)

		go func(quit chan string) {
			_, _ = io.Copy(wrappedrch, lch)
			quit <- "lch"
		}(quit)
	}

	go func(quit chan string) {
		for req := range lreqs {
			b, err := rch.SendRequest(req.Type, req.WantReply, req.Payload)
			if req.Type == "exec" {
				wrappedlch := logchannel.New(lch, f)
				command := append(req.Payload, []byte("\n")...)
				if _, err := wrappedlch.LogWrite(command); err != nil {
					log.Printf("failed to write log: %v", err)
				}
			}

			if err != nil {
				errch <- err
			}
			if err2 := req.Reply(b, nil); err2 != nil {
				errch <- err2
			}
		}
		quit <- "lreqs"
	}(quit)

	go func(quit chan string) {
		for req := range rreqs {
			b, err := lch.SendRequest(req.Type, req.WantReply, req.Payload)
			if err != nil {
				errch <- err
			}
			if err2 := req.Reply(b, nil); err2 != nil {
				errch <- err2
			}
		}
		quit <- "rreqs"
	}(quit)

	lchEOF, rchEOF, lchClosed, rchClosed := false, false, false, false
	for {
		select {
		case err := <-errch:
			return err
		case q := <-quit:
			switch q {
			case "lch":
				lchEOF = true
				_ = rch.CloseWrite()
			case "rch":
				rchEOF = true
				_ = lch.CloseWrite()
			case "lreqs":
				lchClosed = true
			case "rreqs":
				rchClosed = true
			}

			if lchEOF && lchClosed && !rchClosed {
				rch.Close()
			}

			if rchEOF && rchClosed && !lchClosed {
				lch.Close()
			}

			if lchEOF && rchEOF && lchClosed && rchClosed {
				return nil
			}
		}
	}
}
