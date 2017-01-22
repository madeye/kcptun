package main

/*
#include <stdlib.h>
#include <sys/time.h>
#include <sys/types.h>
#include <sys/socket.h>
#include <sys/uio.h>

#define ANCIL_FD_BUFFER(n) \
    struct { \
	struct cmsghdr h; \
	int fd[n]; \
    }

int
ancil_send_fds_with_buffer(int sock, const int *fds, unsigned n_fds, void *buffer)
{
    struct msghdr msghdr;
    char nothing = '!';
    struct iovec nothing_ptr;
    struct cmsghdr *cmsg;
    int i;

    nothing_ptr.iov_base = &nothing;
    nothing_ptr.iov_len = 1;
    msghdr.msg_name = NULL;
    msghdr.msg_namelen = 0;
    msghdr.msg_iov = &nothing_ptr;
    msghdr.msg_iovlen = 1;
    msghdr.msg_flags = 0;
    msghdr.msg_control = buffer;
    msghdr.msg_controllen = sizeof(struct cmsghdr) + sizeof(int) * n_fds;
    cmsg = CMSG_FIRSTHDR(&msghdr);
    cmsg->cmsg_len = msghdr.msg_controllen;
    cmsg->cmsg_level = SOL_SOCKET;
    cmsg->cmsg_type = SCM_RIGHTS;
    for(i = 0; i < n_fds; i++)
	((int *)CMSG_DATA(cmsg))[i] = fds[i];
    return(sendmsg(sock, &msghdr, 0) >= 0 ? 0 : -1);
}

int
ancil_send_fd(int sock, int fd)
{
    ANCIL_FD_BUFFER(1) buffer;

    return(ancil_send_fds_with_buffer(sock, &fd, 1, &buffer));
}

void
set_timeout(int sock)
{
    struct timeval tv;
    tv.tv_sec  = 1;
    tv.tv_usec = 0;
    setsockopt(sock, SOL_SOCKET, SO_RCVTIMEO, (char *)&tv, sizeof(struct timeval));
    setsockopt(sock, SOL_SOCKET, SO_SNDTIMEO, (char *)&tv, sizeof(struct timeval));
}

*/
import "C"

import (
	"crypto/sha1"
	"encoding/csv"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/crypto/pbkdf2"

	"github.com/klauspost/compress/snappy"
	"github.com/pkg/errors"
	"github.com/urfave/cli"
	kcp "github.com/xtaci/kcp-go"
	"github.com/xtaci/smux"
)

var (
	// VERSION is injected by buildflags
	VERSION = "SELFBUILD"
	// SALT is use for pbkdf2 key expansion
	SALT = "kcp-go"
)

type compStream struct {
	conn net.Conn
	w    *snappy.Writer
	r    *snappy.Reader
}

func (c *compStream) Read(p []byte) (n int, err error) {
	return c.r.Read(p)
}

func (c *compStream) Write(p []byte) (n int, err error) {
	n, err = c.w.Write(p)
	err = c.w.Flush()
	return n, err
}

func (c *compStream) Close() error {
	return c.conn.Close()
}

func newCompStream(conn net.Conn) *compStream {
	c := new(compStream)
	c.conn = conn
	c.w = snappy.NewBufferedWriter(conn)
	c.r = snappy.NewReader(conn)
	return c
}

func handleClient(sess *smux.Session, p1 io.ReadWriteCloser) {
	p2, err := sess.OpenStream()
	if err != nil {
		return
	}

	log.Println("stream opened")
	defer log.Println("stream closed")
	defer p1.Close()
	defer p2.Close()

	// start tunnel
	p1die := make(chan struct{})
	go func() { io.Copy(p1, p2); close(p1die) }()

	p2die := make(chan struct{})
	go func() { io.Copy(p2, p1); close(p2die) }()

	// wait for tunnel termination
	select {
	case <-p1die:
	case <-p2die:
	}
}

func checkError(err error) {
	if err != nil {
		log.Printf("%+v\n", err)
		os.Exit(-1)
	}
}

func main() {

	rand.Seed(int64(time.Now().Nanosecond()))
	if VERSION == "SELFBUILD" {
		// add more log flags for debugging
		log.SetFlags(log.LstdFlags | log.Lshortfile)
	}
	myApp := cli.NewApp()
	myApp.Name = "kcptun"
	myApp.Usage = "client(with SMUX)"
	myApp.Version = VERSION
	myApp.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "localaddr,l",
			Value: ":12948",
			Usage: "local listen address",
		},
		cli.StringFlag{
			Name:  "remoteaddr, r",
			Value: "vps:29900",
			Usage: "kcp server address",
		},
		cli.StringFlag{
			Name:   "key",
			Value:  "it's a secrect",
			Usage:  "pre-shared secret between client and server",
			EnvVar: "KCPTUN_KEY",
		},
		cli.StringFlag{
			Name:  "crypt",
			Value: "aes",
			Usage: "aes, aes-128, aes-192, salsa20, blowfish, twofish, cast5, 3des, tea, xtea, xor, none",
		},
		cli.StringFlag{
			Name:  "mode",
			Value: "fast",
			Usage: "profiles: fast3, fast2, fast, normal",
		},
		cli.IntFlag{
			Name:  "conn",
			Value: 1,
			Usage: "set num of UDP connections to server",
		},
		cli.IntFlag{
			Name:  "autoexpire",
			Value: 60,
			Usage: "set auto expiration time(in seconds) for a single UDP connection, 0 to disable",
		},
		cli.IntFlag{
			Name:  "mtu",
			Value: 1350,
			Usage: "set maximum transmission unit for UDP packets",
		},
		cli.IntFlag{
			Name:  "sndwnd",
			Value: 128,
			Usage: "set send window size(num of packets)",
		},
		cli.IntFlag{
			Name:  "rcvwnd",
			Value: 512,
			Usage: "set receive window size(num of packets)",
		},
		cli.IntFlag{
			Name:  "datashard,ds",
			Value: 10,
			Usage: "set reed-solomon erasure coding - datashard",
		},
		cli.IntFlag{
			Name:  "parityshard,ps",
			Value: 3,
			Usage: "set reed-solomon erasure coding - parityshard",
		},
		cli.IntFlag{
			Name:  "dscp",
			Value: 0,
			Usage: "set DSCP(6bit)",
		},
		cli.BoolFlag{
			Name:  "nocomp",
			Usage: "disable compression",
		},
		cli.BoolFlag{
			Name:   "acknodelay",
			Usage:  "flush ack immediately when a packet is received",
			Hidden: true,
		},
		cli.IntFlag{
			Name:   "nodelay",
			Value:  0,
			Hidden: true,
		},
		cli.IntFlag{
			Name:   "interval",
			Value:  40,
			Hidden: true,
		},
		cli.IntFlag{
			Name:   "resend",
			Value:  0,
			Hidden: true,
		},
		cli.IntFlag{
			Name:   "nc",
			Value:  0,
			Hidden: true,
		},
		cli.IntFlag{
			Name:   "sockbuf",
			Value:  4194304, // socket buffer size in bytes
			Hidden: true,
		},
		cli.IntFlag{
			Name:   "keepalive",
			Value:  10, // nat keepalive interval in seconds
			Hidden: true,
		},
		cli.StringFlag{
			Name:  "snmplog",
			Value: "",
			Usage: "collect snmp to file, aware of timeformat in golang, like: ./snmp-20060102.log",
		},
		cli.IntFlag{
			Name:  "snmpperiod",
			Value: 60,
			Usage: "snmp collect period, in seconds",
		},
		cli.StringFlag{
			Name:  "log",
			Value: "",
			Usage: "specify a log file to output, default goes to stderr",
		},
		cli.StringFlag{
			Name:  "c",
			Value: "", // when the value is not empty, the config path must exists
			Usage: "config from json file, which will override the command from shell",
		},
		cli.BoolFlag{
			Name:  "fast-open",
			Usage: "Dummy flag, doesn't really do anything",
		},
		cli.BoolFlag{
			Name:  "V",
			Usage: "Enable VPN mode for shadowsocks-android",
		},
	}
	myApp.Action = func(c *cli.Context) error {
		config := Config{}

		config.LocalAddr = c.String("localaddr")
		config.RemoteAddr = c.String("remoteaddr")
		config.Key = c.String("key")
		config.Crypt = c.String("crypt")
		config.Mode = c.String("mode")
		config.Conn = c.Int("conn")
		config.AutoExpire = c.Int("autoexpire")
		config.MTU = c.Int("mtu")
		config.SndWnd = c.Int("sndwnd")
		config.RcvWnd = c.Int("rcvwnd")
		config.DataShard = c.Int("datashard")
		config.ParityShard = c.Int("parityshard")
		config.DSCP = c.Int("dscp")
		config.NoComp = c.Bool("nocomp")
		config.AckNodelay = c.Bool("acknodelay")
		config.NoDelay = c.Int("nodelay")
		config.Interval = c.Int("interval")
		config.Resend = c.Int("resend")
		config.NoCongestion = c.Int("nc")
		config.SockBuf = c.Int("sockbuf")
		config.KeepAlive = c.Int("keepalive")
		config.Log = c.String("log")
		config.SnmpLog = c.String("snmplog")
		config.SnmpPeriod = c.Int("snmpperiod")

		if c.String("c") != "" {
			err := parseJSONConfig(&config, c.String("c"))
			checkError(err)
		}

		opts, err := parseEnv()
		if err == nil {
			fmt.Printf("test")
			if c, b := opts.Get("localaddr"); b {
				config.LocalAddr = c
			}
			if c, b := opts.Get("remoteaddr"); b {
				config.RemoteAddr = c
			}
			if c, b := opts.Get("key"); b {
				config.Key = c
			}
			if c, b := opts.Get("crypt"); b {
				config.Crypt = c
			}
			if c, b := opts.Get("mode"); b {
				config.Mode = c
			}
			if c, b := opts.Get("conn"); b {
				if conn, err := strconv.Atoi(c); err == nil {
					config.Conn = conn
				}
			}
			if c, b := opts.Get("autoexpire"); b {
				if autoexpire, err := strconv.Atoi(c); err == nil {
					config.AutoExpire = autoexpire
				}
			}
			if c, b := opts.Get("mtu"); b {
				if mtu, err := strconv.Atoi(c); err == nil {
					config.MTU = mtu
				}
			}
			if c, b := opts.Get("sndwnd"); b {
				if sndwnd, err := strconv.Atoi(c); err == nil {
					config.SndWnd = sndwnd
				}
			}
			if c, b := opts.Get("rcvwnd"); b {
				if rcvwnd, err := strconv.Atoi(c); err == nil {
					config.RcvWnd = rcvwnd
				}
			}
			if c, b := opts.Get("datashard"); b {
				if datashard, err := strconv.Atoi(c); err == nil {
					config.DataShard = datashard
				}
			}
			if c, b := opts.Get("parityshard"); b {
				if parityshard, err := strconv.Atoi(c); err == nil {
					config.ParityShard = parityshard
				}
			}
			if c, b := opts.Get("dscp"); b {
				if dscp, err := strconv.Atoi(c); err == nil {
					config.DSCP = dscp
				}
			}
			if c, b := opts.Get("nocomp"); b {
				if nocomp, err := strconv.ParseBool(c); err == nil {
					config.NoComp = nocomp
				}
			}
			if c, b := opts.Get("acknodelay"); b {
				if acknodelay, err := strconv.ParseBool(c); err == nil {
					config.AckNodelay = acknodelay
				}
			}
			if c, b := opts.Get("nodelay"); b {
				if nodelay, err := strconv.Atoi(c); err == nil {
					config.NoDelay = nodelay
				}
			}
			if c, b := opts.Get("interval"); b {
				if interval, err := strconv.Atoi(c); err == nil {
					config.Interval = interval
				}
			}
			if c, b := opts.Get("resend"); b {
				if resend, err := strconv.Atoi(c); err == nil {
					config.Resend = resend
				}
			}
			if c, b := opts.Get("nc"); b {
				if nc, err := strconv.Atoi(c); err == nil {
					config.NoCongestion = nc
				}
			}
			if c, b := opts.Get("sockbuf"); b {
				if sockbuf, err := strconv.Atoi(c); err == nil {
					config.SockBuf = sockbuf
				}
			}
			if c, b := opts.Get("keepalive"); b {
				if keepalive, err := strconv.Atoi(c); err == nil {
					config.KeepAlive = keepalive
				}
			}
			if c, b := opts.Get("log"); b {
				config.Log = c
			}
			if c, b := opts.Get("snmplog"); b {
				config.SnmpLog = c
			}
			if c, b := opts.Get("snmpperiod"); b {
				if snmpperiod, err := strconv.Atoi(c); err == nil {
					config.SnmpPeriod = snmpperiod
				}
			}
		}

		// log redirect
		if config.Log != "" {
			f, err := os.OpenFile(config.Log, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
			checkError(err)
			defer f.Close()
			log.SetOutput(f)
		}

		switch config.Mode {
		case "normal":
			config.NoDelay, config.Interval, config.Resend, config.NoCongestion = 0, 30, 2, 1
		case "fast":
			config.NoDelay, config.Interval, config.Resend, config.NoCongestion = 0, 20, 2, 1
		case "fast2":
			config.NoDelay, config.Interval, config.Resend, config.NoCongestion = 1, 20, 2, 1
		case "fast3":
			config.NoDelay, config.Interval, config.Resend, config.NoCongestion = 1, 10, 2, 1
		}

		log.Println("version:", VERSION)
		addr, err := net.ResolveTCPAddr("tcp", config.LocalAddr)
		checkError(err)
		listener, err := net.ListenTCP("tcp", addr)
		checkError(err)

		pass := pbkdf2.Key([]byte(config.Key), []byte(SALT), 4096, 32, sha1.New)
		var block kcp.BlockCrypt
		switch config.Crypt {
		case "tea":
			block, _ = kcp.NewTEABlockCrypt(pass[:16])
		case "xor":
			block, _ = kcp.NewSimpleXORBlockCrypt(pass)
		case "none":
			block, _ = kcp.NewNoneBlockCrypt(pass)
		case "aes-128":
			block, _ = kcp.NewAESBlockCrypt(pass[:16])
		case "aes-192":
			block, _ = kcp.NewAESBlockCrypt(pass[:24])
		case "blowfish":
			block, _ = kcp.NewBlowfishBlockCrypt(pass)
		case "twofish":
			block, _ = kcp.NewTwofishBlockCrypt(pass)
		case "cast5":
			block, _ = kcp.NewCast5BlockCrypt(pass[:16])
		case "3des":
			block, _ = kcp.NewTripleDESBlockCrypt(pass[:24])
		case "xtea":
			block, _ = kcp.NewXTEABlockCrypt(pass[:16])
		case "salsa20":
			block, _ = kcp.NewSalsa20BlockCrypt(pass)
		default:
			config.Crypt = "aes"
			block, _ = kcp.NewAESBlockCrypt(pass)
		}

		log_init()

		log.Println("listening on:", listener.Addr())
		log.Println("encryption:", config.Crypt)
		log.Println("nodelay parameters:", config.NoDelay, config.Interval, config.Resend, config.NoCongestion)
		log.Println("remote address:", config.RemoteAddr)
		log.Println("sndwnd:", config.SndWnd, "rcvwnd:", config.RcvWnd)
		log.Println("compression:", !config.NoComp)
		log.Println("mtu:", config.MTU)
		log.Println("datashard:", config.DataShard, "parityshard:", config.ParityShard)
		log.Println("acknodelay:", config.AckNodelay)
		log.Println("dscp:", config.DSCP)
		log.Println("sockbuf:", config.SockBuf)
		log.Println("keepalive:", config.KeepAlive)
		log.Println("conn:", config.Conn)
		log.Println("autoexpire:", config.AutoExpire)
		log.Println("snmplog:", config.SnmpLog)
		log.Println("snmpperiod:", config.SnmpPeriod)

		if config.Vpn {

			path := "protect_path"

			callback := func(fd int) {
				socket, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
				if err != nil {
					log.Println(err)
					return
				}
				defer syscall.Close(socket)

				C.set_timeout(C.int(socket))

				err = syscall.Connect(socket, &syscall.SockaddrUnix{Name: path})
				if err != nil {
					log.Println(err)
					return
				}

				C.ancil_send_fd(C.int(socket), C.int(fd))

				dummy := []byte{1}
				n, err := syscall.Read(socket, dummy)
				if err != nil {
					log.Println(err)
					return
				}
				if n != 1 {
					log.Println("Failed to protect fd: ", fd)
					return
				}
			}

			SetNetCallback(callback)

		}

		smuxConfig := smux.DefaultConfig()
		smuxConfig.MaxReceiveBuffer = config.SockBuf

		createConn := func() (*smux.Session, error) {
			kcpconn, err := kcp.DialWithOptions(config.RemoteAddr, block, config.DataShard, config.ParityShard)
			if err != nil {
				return nil, errors.Wrap(err, "createConn()")
			}
			kcpconn.SetStreamMode(true)
			kcpconn.SetNoDelay(config.NoDelay, config.Interval, config.Resend, config.NoCongestion)
			kcpconn.SetWindowSize(config.SndWnd, config.RcvWnd)
			kcpconn.SetMtu(config.MTU)
			kcpconn.SetACKNoDelay(config.AckNodelay)
			kcpconn.SetKeepAlive(config.KeepAlive)

			if err := kcpconn.SetDSCP(config.DSCP); err != nil {
				log.Println("SetDSCP:", err)
			}
			if err := kcpconn.SetReadBuffer(config.SockBuf); err != nil {
				log.Println("SetReadBuffer:", err)
			}
			if err := kcpconn.SetWriteBuffer(config.SockBuf); err != nil {
				log.Println("SetWriteBuffer:", err)
			}

			// stream multiplex
			var session *smux.Session
			if config.NoComp {
				session, err = smux.Client(kcpconn, smuxConfig)
			} else {
				session, err = smux.Client(newCompStream(kcpconn), smuxConfig)
			}
			if err != nil {
				return nil, errors.Wrap(err, "createConn()")
			}
			return session, nil
		}

		// wait until a connection is ready
		waitConn := func() *smux.Session {
			for {
				if session, err := createConn(); err == nil {
					return session
				} else {
					time.Sleep(time.Second)
				}
			}
		}

		numconn := uint16(config.Conn)
		muxes := make([]struct {
			session *smux.Session
			ttl     time.Time
		}, numconn)

		for k := range muxes {
			sess, err := createConn()
			checkError(err)
			muxes[k].session = sess
			muxes[k].ttl = time.Now().Add(time.Duration(config.AutoExpire) * time.Second)
		}

		chScavenger := make(chan *smux.Session, 128)
		go scavenger(chScavenger)
		go snmpLogger(config.SnmpLog, config.SnmpPeriod)
		rr := uint16(0)
		for {
			p1, err := listener.AcceptTCP()
			if err != nil {
				log.Fatalln(err)
			}
			if err := p1.SetReadBuffer(config.SockBuf); err != nil {
				log.Println("TCP SetReadBuffer:", err)
			}
			if err := p1.SetWriteBuffer(config.SockBuf); err != nil {
				log.Println("TCP SetWriteBuffer:", err)
			}
			checkError(err)
			idx := rr % numconn

			// do auto expiration && reconnection
			if muxes[idx].session.IsClosed() || (config.AutoExpire > 0 && time.Now().After(muxes[idx].ttl)) {
				chScavenger <- muxes[idx].session
				muxes[idx].session = waitConn()
				muxes[idx].ttl = time.Now().Add(time.Duration(config.AutoExpire) * time.Second)
			}

			go handleClient(muxes[idx].session, p1)
			rr++
		}
	}
	myApp.Run(os.Args)
}

type scavengeSession struct {
	session *smux.Session
	ttl     time.Time
}

const (
	maxScavengeTTL = 10 * time.Minute
)

func scavenger(ch chan *smux.Session) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	var sessionList []scavengeSession
	for {
		select {
		case sess := <-ch:
			sessionList = append(sessionList, scavengeSession{sess, time.Now()})
		case <-ticker.C:
			var newList []scavengeSession
			for k := range sessionList {
				s := sessionList[k]
				if s.session.NumStreams() == 0 || s.session.IsClosed() || time.Since(s.ttl) > maxScavengeTTL {
					log.Println("session scavenged")
					s.session.Close()
				} else {
					newList = append(newList, sessionList[k])
				}
			}
			sessionList = newList
		}
	}
}

func snmpLogger(path string, interval int) {
	if path == "" || interval == 0 {
		return
	}
	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			f, err := os.OpenFile(time.Now().Format(path), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
			if err != nil {
				log.Println(err)
				return
			}
			w := csv.NewWriter(f)
			// write header in empty file
			if stat, err := f.Stat(); err == nil && stat.Size() == 0 {
				if err := w.Write(append([]string{"Unix"}, kcp.DefaultSnmp.Header()...)); err != nil {
					log.Println(err)
				}
			}
			if err := w.Write(append([]string{fmt.Sprint(time.Now().Unix())}, kcp.DefaultSnmp.ToSlice()...)); err != nil {
				log.Println(err)
			}
			kcp.DefaultSnmp.Reset()
			w.Flush()
			f.Close()
		}
	}
}
