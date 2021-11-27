package main

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/XciD/unifi-protect/pkg/protect"
	"github.com/cloudwebrtc/go-sip-ua/pkg/account"
	"github.com/cloudwebrtc/go-sip-ua/pkg/media/rtp"
	"github.com/cloudwebrtc/go-sip-ua/pkg/session"
	"github.com/cloudwebrtc/go-sip-ua/pkg/stack"
	"github.com/cloudwebrtc/go-sip-ua/pkg/ua"
	"github.com/cloudwebrtc/go-sip-ua/pkg/utils"
	"github.com/ghettovoice/gosip/log"
	"github.com/ghettovoice/gosip/sip"
	"github.com/ghettovoice/gosip/sip/parser"
	"github.com/pixelbender/go-sdp/sdp"
	"github.com/spf13/viper"
)

var (
	logger     log.Logger
	rtpSession map[string]*RTPPiper
	ffmpeg     = ""
)

func init() {
	viper.SetConfigName("config")
	viper.AddConfigPath(".")
	viper.SetConfigType("yaml")
	viper.AutomaticEnv()
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "__", "-", "_"))

	if err := viper.ReadInConfig(); err != nil {
		panic(err)
	}

	ffmpeg = viper.GetString("ffmpeg")
	rand.Seed(time.Now().UTC().UnixNano())
	rtpSession = make(map[string]*RTPPiper)
	logger = utils.NewLogrusLogger(log.InfoLevel, "Client", nil)
}

func main() {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT, syscall.SIGKILL)

	// Setup the NVR
	nvr := protect.NewNVR(
		viper.GetString("nvr.host"),
		viper.GetInt("nvr.port"),
		viper.GetString("nvr.user"),
		viper.GetString("nvr.password"))

	// Start the NVR Livefeed
	if err := nvr.Authenticate(); err != nil {
		logger.Fatal(err)
	}

	bootstrap, err := nvr.GetBootstrap()
	if err != nil {
		logger.Fatal(err)
	}

	camera := bootstrap.GetCameraID(viper.GetString("camera.id"))
	channel := viper.GetInt("camera.channel")

	// Set up the sip stack
	stack := stack.NewSipStack(&stack.SipStackConfig{
		UserAgent: "Go Sip Client/example-client"},
	)
	stack.Log().SetLevel(log.ErrorLevel)

	listen := "0.0.0.0:5080"
	logger.Infof("Listen => %s", listen)

	if err := stack.Listen("udp", listen); err != nil {
		logger.Fatal(err)
	}

	if err := stack.Listen("tcp", listen); err != nil {
		logger.Fatal(err)
	}

	ua := ua.NewUserAgent(&ua.UserAgentConfig{SipStack: stack})

	ua.InviteStateHandler = func(sess *session.Session, req *sip.Request, resp *sip.Response, state session.Status) {
		sess.Log().Infof("%s: %s", sess.CallID(), state)
		switch state {
		case session.InviteReceived:
			callID := sess.CallID().String()

			if rtpPiper, ok := rtpSession[callID]; ok {
				sess.ProvideAnswer(rtpPiper.Answer())
				sess.Accept(200)
				return
			}

			offerSDP := (*req).Body()
			offerSDPSession, err := sdp.ParseString(offerSDP)
			if err != nil {
				sess.Reject(500, err.Error())
				return
			}
			sess.ProvideOffer(offerSDPSession.String())

			logger.Infof(offerSDPSession.String())

			internalIP := (*req).Recipient().Host()
			rtpPiper := NewRTPPiper(internalIP, offerSDPSession, nvr, camera, channel)
			if err := rtpPiper.Start(); err != nil {
				sess.Reject(500, err.Error())
				return
			}

			sess.ProvideAnswer(rtpPiper.Answer())
			sess.Accept(200)

			rtpSession[callID] = rtpPiper

		case session.Canceled:
			return
		case session.Failure:
			fallthrough
		case session.Terminated:
			callID := sess.CallID().String()

			if rtpPiper, ok := rtpSession[callID]; ok {
				if err := rtpPiper.Close(); err != nil {
					logger.Errorf("Error closing rtpPipe %s", err.Error())
				}
				delete(rtpSession, callID)
			}
		}
	}

	ua.RegisterStateHandler = func(state account.RegisterState) {
		if state.StatusCode != 200 {
			logger.Fatal("Register did not return 200")
		}

		logger.Infof("RegisterStateHandler: user => %s, state => %v, expires => %v",
			state.Account.AuthInfo.AuthUser, state.StatusCode, state.Expiration)
	}

	sipURI := fmt.Sprintf("sip:%s@%s", viper.GetString("sip.user"), viper.GetString("sip.server"))

	uri, err := parser.ParseUri(sipURI)
	if err != nil {
		logger.Fatal(err)
	}

	profile := account.NewProfile(uri.Clone(), "goSIP/example-client",
		&account.AuthInfo{
			AuthUser: viper.GetString("sip.user"),
			Password: viper.GetString("sip.password"),
			Realm:    viper.GetString("sip.server"),
		},
		600,
		stack,
	)

	recipient, err := parser.ParseSipUri(sipURI)
	if err != nil {
		logger.Fatal(err)
	}

	register, err := ua.SendRegister(profile, recipient, profile.Expires, nil)

	if err != nil {
		logger.Fatal(err)
	}

	<-stop

	logger.Warnf("Exiting")
	register.SendRegister(0)

	ua.Shutdown()
}

func getRandomPort() int {
	return rand.Intn(40000) + 20000
}

type RTPPiper struct {
	sync.Mutex
	piper                  *rtp.RtpUDPStream // The pipe that send to input or to the client
	piperRtcp              *rtp.RtpUDPStream
	commandIn              *exec.Cmd
	commandOut             *exec.Cmd
	liveFeed               *protect.LiveFeed
	logger                 log.Logger
	externalAddress        net.UDPAddr
	inputAddress           net.UDPAddr
	externalControlAddress net.UDPAddr
	inputControlAddress    net.UDPAddr
	stop                   bool
	internalIP             net.IP
	nvr                    *protect.NVR
	camera                 *protect.Camera
	cameraChannel          int
	offerSDPSession        *sdp.Session
}

func (p *RTPPiper) Close() error {
	if !p.stop {
		p.stop = true
		p.logger.Warnf("Closing")
		p.piper.Close()
		p.piperRtcp.Close()
		if err := p.liveFeed.Close(); err != nil {
			return err
		}
		if err := p.commandIn.Process.Signal(syscall.SIGTERM); err != nil {
			return err
		}
		if err := p.commandOut.Process.Signal(syscall.SIGTERM); err != nil {
			return err
		}
		p.logger.Warnf("Closed")
	}
	return nil
}

func NewRTPPiper(internalIP string, offerSDPSession *sdp.Session, nvr *protect.NVR, camera *protect.Camera, channel int) *RTPPiper {
	externalUDPServer := net.UDPAddr{
		IP:   net.ParseIP(offerSDPSession.Media[0].Connection[0].Address),
		Port: offerSDPSession.Media[0].Port,
	}

	internalUDPServer := net.UDPAddr{
		IP:   net.ParseIP(internalIP),
		Port: getRandomPort(),
	}

	return &RTPPiper{
		offerSDPSession: offerSDPSession,
		internalIP:      internalUDPServer.IP,
		externalAddress: externalUDPServer,
		externalControlAddress: net.UDPAddr{
			IP:   externalUDPServer.IP,
			Port: externalUDPServer.Port + 1,
			Zone: externalUDPServer.Zone,
		},
		inputAddress: internalUDPServer,
		inputControlAddress: net.UDPAddr{
			IP:   internalUDPServer.IP,
			Port: internalUDPServer.Port + 1,
			Zone: internalUDPServer.Zone,
		},
		nvr:           nvr,
		camera:        camera,
		cameraChannel: channel,
		logger:        utils.NewLogrusLogger(log.InfoLevel, "RTPPiper", nil),
	}
}

func (p *RTPPiper) Start() error {
	p.piper = rtp.NewRtpUDPStream(p.internalIP.String(), 10000, 20000, func(pkt []byte, raddr net.Addr) {
		// If packet came from external => transfer it to input udp
		if raddr.String() == p.externalAddress.String() {
			if _, err := p.piper.Send(pkt, &p.inputAddress); err != nil {
				p.logger.Error("Error sending data to inputAddress %s", err.Error())
			}
		} else {
			if _, err := p.piper.Send(pkt, &p.externalAddress); err != nil {
				p.logger.Error("Error sending data to externalAddress %s", err.Error())
			}
		}
	})

	piperPort := p.piper.LocalAddr().Port

	// RTCP port is +1 compare to the RTP Server
	p.piperRtcp = rtp.NewRtpUDPStream(p.internalIP.String(), piperPort+1, piperPort+1, func(pkt []byte, raddr net.Addr) {
		p.logger.Infof("Received a Control packet from %s", raddr)
		if raddr.String() == p.externalControlAddress.String() {
			if _, err := p.piperRtcp.Send(pkt, &p.inputControlAddress); err != nil {
				p.logger.Error("Error sending data to inputAddress %s", err.Error())
			}
		} else {
			if _, err := p.piperRtcp.Send(pkt, &p.externalControlAddress); err != nil {
				p.logger.Error("Error sending data to externalAddress %s", err.Error())
			}
		}
	})

	randomPortLiveFeed := getRandomPort()

	// Cmd IN will read from Doorbell and forward to the internal server that will push it to the external server
	p.commandIn = exec.Command(ffmpeg,
		"-hide_banner",
		"-i", fmt.Sprintf("tcp://%s:%d?listen", p.internalIP.String(), randomPortLiveFeed),
		"-map", "0:a",
		//"-acodec", "pcm_alaw",
		"-acodec", "libspeex",
		"-ar", "16000",
		"-flags", "+global_header",
		"-af", "highpass=f=200,lowpass=f=1000",
		//"-payload_type", "8",
		"-payload_type", "98",
		"-f", "rtp",
		fmt.Sprintf("rtp://%s:%d", p.internalIP.String(), p.piper.LocalAddr().Port),
	)

	p.logger.WithPrefix("CMDIN").Infof("%s", strings.Join(p.commandIn.Args, " "))

	cmdReader, err := p.commandIn.StderrPipe()
	if err != nil {
		return err
	}
	scannerIn := bufio.NewScanner(cmdReader)
	scannerInChannel := make(chan string)
	go readScanner(scannerIn, "CMDIN", scannerInChannel)

	err = ioutil.WriteFile("sdp", []byte(fmt.Sprintf(`v=0
o=- 0 0 IN IP4 %s
s=Impact Moderato
c=IN IP4 %s
t=0 0
a=tool:libavformat 58.76.100
m=audio %d RTP/AVP 98
a=rtpmap:98 speex/16000
`, p.internalIP.String(), p.internalIP.String(), p.inputAddress.Port)), 0600)

	if err != nil {
		return err
	}

	p.commandOut = exec.Command(ffmpeg,
		"-hide_banner",
		"-protocol_whitelist", "file,crypto,udp,rtp",
		"-i", "sdp",
		"-acodec", p.camera.TalkbackSettings.TypeFmt,
		"-flags", "+global_header",
		"-ac", fmt.Sprintf("%d", p.camera.TalkbackSettings.Channels),
		"-ar", fmt.Sprintf("%d", p.camera.TalkbackSettings.SamplingRate),
		"-b:a", "64k",
		"-f", "adts",
		fmt.Sprintf("udp://%s:%d", p.camera.Host, p.camera.TalkbackSettings.BindPort),
	)

	p.logger.WithPrefix("CMDOUT").Infof("%s", strings.Join(p.commandOut.Args, " "))

	cmdReaderOut, err := p.commandOut.StderrPipe()
	if err != nil {
		return err
	}
	scannerOut := bufio.NewScanner(cmdReaderOut)
	scannerOutChannel := make(chan string)
	go readScanner(scannerOut, "CMDOUT", scannerOutChannel)

	// We send two empty bytes to open the connection
	p.piper.Send([]byte(""), &p.externalAddress)
	p.piperRtcp.Send([]byte(""), &p.externalControlAddress)

	go p.piper.Read()
	go p.piperRtcp.Read()

	if err := p.commandOut.Start(); err != nil {
		return err
	}

	if err := p.commandIn.Start(); err != nil {
		return err
	}

	go func() {
		if err := p.commandOut.Wait(); err != nil {
			p.logger.WithPrefix("CMDOUT").Errorf(err.Error())
		}
		p.logger.WithPrefix("CMDOUT").Warnf("STOPPED")
		_ = p.Close()
	}()

	go func() {
		if err := p.commandIn.Wait(); err != nil {
			p.logger.WithPrefix("CMDIN").Errorf(err.Error())
		}
		p.logger.WithPrefix("CMDIN").Warnf("STOPPED")
		_ = p.Close()
	}()

	p.liveFeed = p.nvr.GetLiveFeed(p.camera.ID, p.cameraChannel)

	go p.liveFeed.PumpData()

	var feedIn *net.TCPConn
	for {
		logrus.Infof("Waiting for input in %d", randomPortLiveFeed)
		time.Sleep(100 * time.Millisecond)
		feedIn, err = net.DialTCP("tcp", nil, &net.TCPAddr{
			IP:   net.ParseIP(p.internalIP.String()),
			Port: randomPortLiveFeed,
		})
		if err != nil {
			continue
		}
		break
	}

	go func() {
		for {
			select {
			case cmd, ok := <-scannerOutChannel:
				if !ok {
					return
				}
				p.logger.Info(cmd)
			case cmd, ok := <-scannerInChannel:
				if !ok {
					return
				}
				p.logger.Info(cmd)
			case fragment := <-p.liveFeed.Events:
				if len(fragment) == 0 {
					p.logger.Infof("Closing the livefeed")
					return
				}
				if _, err := feedIn.Write(fragment); err != nil {
					p.logger.Errorf("Error writing to TCP Addr %s", err.Error())
					return
				}
			}
		}
	}()

	return nil
}

func (p *RTPPiper) Answer() string {
	return fmt.Sprintf(`v=0
o=mobotix 123456 654321 IN IP4 %s
s=A conversation
c=IN IP4 %s
t=0 0
m=audio %d RTP/AVP 98
a=sendrecv
a=rtpmap:98 speex/16000
`, p.internalIP.String(), p.internalIP.String(), p.piper.LocalAddr().Port)
}

func readScanner(scanner *bufio.Scanner, prefix string, lines chan string) {
	for scanner.Scan() {
		lines <- fmt.Sprintf("%s: %s", prefix, scanner.Text())
	}
	close(lines)
}
