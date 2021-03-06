package main

import (
	"bufio"
	"bytes"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bookingcom/nanotube/pkg/conf"
	"github.com/bookingcom/nanotube/pkg/metrics"
	"github.com/libp2p/go-reuseport"

	"github.com/facebookgo/grace/gracenet"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

// Listen listens for incoming metric data
func Listen(n *gracenet.Net, cfg *conf.Main, stop <-chan struct{}, lg *zap.Logger, ms *metrics.Prom) (chan string, error) {
	queue := make(chan string, cfg.MainQueueSize)
	var connWG sync.WaitGroup

	if cfg.ListenTCP != "" {
		ip, port, err := parseListenOption(cfg.ListenTCP)
		if err != nil {
			return nil, errors.Wrap(err, "error parsing ListenTCP option")
		}
		l, err := n.ListenTCP("tcp", &net.TCPAddr{
			IP:   ip,
			Port: port,
		})
		if err != nil {
			return nil, errors.Wrap(err,
				"error while opening TCP port for listening")
		}
		lg.Info("Launch: Opened TCP connection for listening.", zap.String("ListenTCP", cfg.ListenTCP))

		connWG.Add(1)
		go acceptAndListenTCP(l, queue, stop, cfg, &connWG, ms, lg)
	}

	if cfg.ListenUDP != "" {
		conn, err := reuseport.ListenPacket("udp", cfg.ListenUDP)

		if err != nil {
			lg.Error("error while opening UDP port for listening", zap.Error(err))
			return nil, errors.Wrap(err, "error while opening UDP connection")
		}
		lg.Info("Launch: Opened UDP connection for listening.", zap.String("ListenUDP", cfg.ListenUDP))

		connWG.Add(1)
		go listenUDP(conn, queue, stop, &connWG, ms, lg)
	}

	go func() {
		connWG.Wait()
		lg.Info("Termination: All incoming connections closed. Draining the main queue.")
		close(queue)
	}()

	return queue, nil
}

func acceptAndListenTCP(l net.Listener, queue chan string, term <-chan struct{},
	cfg *conf.Main, connWG *sync.WaitGroup, ms *metrics.Prom, lg *zap.Logger) {
	var wg sync.WaitGroup

loop:
	for {
		connCh := make(chan net.Conn)
		errCh := make(chan error)
		go func() {
			conn, err := l.Accept()

			ms.ActiveTCPConnections.Inc()
			ms.InConnectionsTotalTCP.Inc()

			if err != nil {
				errCh <- err
			} else {
				connCh <- conn
			}
		}()

		select {
		case <-term:
			// stop accepting new connections on termination signal
			err := l.Close()
			if err != nil {
				lg.Error("failed to close listening TCP connection", zap.Error(err))
			}
			break loop
		case err := <-errCh:
			lg.Error("accepting connection failed", zap.Error(err))
		case conn := <-connCh:
			wg.Add(1)
			go readFromConnectionTCP(&wg, conn, queue, term, cfg, ms, lg)
		}
	}
	lg.Info("Termination: Stopped accepting new TCP connections. Starting to close incoming connections...")
	wg.Wait()
	lg.Info("Termination: Finished previously accpted TCP connections.")
	connWG.Done()
}

func listenUDP(conn net.PacketConn, queue chan string, stop <-chan struct{}, connWG *sync.WaitGroup, ms *metrics.Prom, lg *zap.Logger) {
	go func() {
		<-stop
		lg.Info("Termination: Closing the UDP connection.")
		cerr := conn.Close()
		if cerr != nil {
			lg.Error("closing the incoming UDP connection failed", zap.Error(cerr))
		}
	}()

	buf := make([]byte, 64*1024) // 64k is the max UDP datagram size
	for {
		nRead, _, err := conn.ReadFrom(buf)
		if err != nil {
			// There is no other way, see https://github.com/golang/go/issues/4373
			if strings.Contains(err.Error(), "use of closed network connection") {
				break
			}

			lg.Error("error reading UDP datagram", zap.Error(err))
			continue
		}

		// WARNING: The split does not copy the data.
		lines := bytes.Split(buf[:nRead], []byte{'\n'})

		// TODO (grzkv): string -> []bytes, line has to be copied to avoid races.
		for i := 0; i < len(lines)-1; i++ {
			sendToMainQ(string(lines[i]), queue, ms)
		}
	}

	lg.Info("Termination: Stopped accepting UDP data.")
	connWG.Done()
}

func sendToMainQ(rec string, q chan<- string, ms *metrics.Prom) {
	select {
	case q <- rec:
		ms.InRecs.Inc()
	default:
		ms.ThrottledRecs.Inc()
	}
}

func readFromConnectionTCP(wg *sync.WaitGroup, conn net.Conn, queue chan string, stop <-chan struct{}, cfg *conf.Main, ms *metrics.Prom, lg *zap.Logger) {
	defer wg.Done() // executed after the connection is closed
	defer func() {
		err := conn.Close()
		if err != nil {
			lg.Error("closing the incoming connection", zap.Error(err))
		}
		ms.ActiveTCPConnections.Dec()
	}()

	// what if client connects and does nothing? protect!
	err := conn.SetReadDeadline(time.Now().Add(
		time.Duration(cfg.IncomingConnIdleTimeoutSec) * time.Second))
	if err != nil {
		lg.Error("error setting read deadline",
			zap.Error(err),
			zap.String("sender", conn.RemoteAddr().String()))
	}

	scanForRecordsTCP(conn, queue, stop, cfg, ms, lg)
}

func scanForRecordsTCP(conn net.Conn, queue chan string, stop <-chan struct{}, cfg *conf.Main, ms *metrics.Prom, lg *zap.Logger) {
	sc := bufio.NewScanner(conn)
	scanin := make(chan string)
	go func() {
		for sc.Scan() {
			scanin <- sc.Text()
		}
		close(scanin)
	}()

loop:
	for {
		select {
		case rec, open := <-scanin:
			if !open {
				break loop
			} else {
				// what if client connects and does nothing? protect!
				err := conn.SetReadDeadline(time.Now().Add(
					time.Duration(cfg.IncomingConnIdleTimeoutSec) * time.Second))
				if err != nil {
					lg.Error("error setting read deadline",
						zap.Error(err),
						zap.String("sender", conn.RemoteAddr().String()))
				}

				sendToMainQ(rec, queue, ms)
			}
		case <-stop:
			// give the reader the ability to drain the queue and close afterwards
			break loop // break both from select and from for
		}
	}
}

// parse "ip:port" string that is used for ListenTCP and ListenUDP config options
func parseListenOption(listenOption string) (net.IP, int, error) {
	ipStr, portStr, err := net.SplitHostPort(listenOption)
	if err != nil {
		return nil, 0, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, port, err
	}
	if port < 0 || port > 65535 {
		return nil, port, errors.New("invalid port value")
	}
	// ":2003" will listen on all IPs
	if ipStr == "" {
		return nil, port, nil
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return ip, port, errors.New("could not parse IP")
	}
	return ip, port, nil
}
