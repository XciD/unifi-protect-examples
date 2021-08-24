package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/XciD/unifi-protect/pkg/protect"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

var (
	ffmpeg = ""
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
}

func main() {
	// Setup the NVR
	nvr := protect.NewNVR(
		viper.GetString("nvr.host"),
		viper.GetInt("nvr.port"),
		viper.GetString("nvr.user"),
		viper.GetString("nvr.password"))

	// Start the NVR Livefeed
	if err := nvr.Authenticate(); err != nil {
		logrus.Fatal(err)
	}

	bootstrap, err := nvr.GetBootstrap()
	if err != nil {
		logrus.Fatal(err)
	}

	extractors := make(map[string]*ImageExtractor)

	// TODO: Create an adaptive channel if incoming ip came from internal network
	channelToRead := viper.GetInt("camera.channel")

	for _, c := range bootstrap.Cameras {
		camera := c
		extractors[camera.ID] = NewImageExtractor(
			nvr,
			&camera,
			channelToRead,
			10*time.Second)
	}

	e := echo.New()
	e.Use(middleware.LoggerWithConfig(middleware.LoggerConfig{
		Format: "method=${method}, uri=${uri}, status=${status}\n",
	}))
	e.Use(middleware.Recover())
	e.Use(middleware.GzipWithConfig(middleware.GzipConfig{
		Level: 1,
	}))

	e.GET(":camera", func(c echo.Context) error {
		param := c.Param("camera")
		if e, ok := extractors[param]; ok {
			img, err := e.GetLastImg()
			if err != nil {
				logrus.Errorf(err.Error())
				return err
			}
			return c.Blob(http.StatusOK, "image/jpeg", img)
		}
		return c.NoContent(http.StatusNotFound)
	})

	if err := e.Start(":8080"); err != nil {
		panic(err)
	}
}

type ImageExtractor struct {
	sync.Mutex
	timer           *time.Timer
	running         bool
	starting        bool
	nvr             *protect.NVR
	feed            *protect.LiveFeed
	lastImg         []byte
	camera          *protect.Camera
	outputTCPServer *net.TCPListener
	channel         int
	ffmpegCmd       *exec.Cmd
	duration        time.Duration
	inputPort       int
	outputPort      int
}

func NewImageExtractor(nvr *protect.NVR, camera *protect.Camera, channel int, duration time.Duration) *ImageExtractor {
	timer := time.NewTimer(duration)
	timer.Stop()
	return &ImageExtractor{
		nvr:        nvr,
		timer:      timer,
		duration:   duration,
		camera:     camera,
		channel:    channel,
		inputPort:  getRandomPort(),
		outputPort: getRandomPort(),
	}
}

func (i *ImageExtractor) close() {
	i.lastImg = nil
	i.running = false
	i.starting = false
	if i.ffmpegCmd != nil {
		_ = i.ffmpegCmd.Process.Kill()
		i.ffmpegCmd = nil
	}
	if i.outputTCPServer != nil {
		_ = i.outputTCPServer.Close()
		i.outputTCPServer = nil
	}
}

func (i *ImageExtractor) start() error {
	outputTCPServer, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("0.0.0.0"), Port: i.outputPort})

	if err != nil {
		return err
	}

	i.outputTCPServer = outputTCPServer

	feed := i.nvr.GetLiveFeed(i.camera.ID, i.channel)

	i.ffmpegCmd = exec.Command(ffmpeg,
		"-hide_banner",
		"-i", fmt.Sprintf("tcp://0.0.0.0:%d?listen", i.inputPort),
		"-f", "image2pipe", fmt.Sprintf("tcp://0.0.0.0:%d", i.outputPort),
	)

	cmdReaderOut, err := i.ffmpegCmd.StderrPipe()
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(cmdReaderOut)
	cmdStdErr := make(chan string)
	go readScanner(scanner, cmdStdErr)

	if err := i.ffmpegCmd.Start(); err != nil {
		return err
	}

	processDone := make(chan struct{})
	go func() {
		defer func() {
			logrus.Infof("Closing wait loop")
		}()
		i.ffmpegCmd.Wait()
		close(processDone)
	}()

	go feed.PumpData()

	// Wait for the input connection
	var inputConnection *net.TCPConn
	for {
		logrus.Infof("Waiting for input in %d", i.inputPort)
		time.Sleep(500 * time.Millisecond)
		inputConnection, err = net.DialTCP("tcp", nil, &net.TCPAddr{
			IP:   net.ParseIP("0.0.0.0"),
			Port: i.inputPort,
		})
		if err != nil {
			continue
		}
		break
	}

	go func() {
		defer func() {
			logrus.Infof("Closing main loop")
			i.close()
			_ = inputConnection.Close()
		}()
		for {
			select {
			case <-i.timer.C:
				return
			case <-processDone:
				return
			case cmd, ok := <-cmdStdErr:
				if !ok {
					return
				}
				logrus.Debugf("CMD: %s", cmd)
			case fragment := <-feed.Events:
				if len(fragment) == 0 {
					return
				}
				i.running = true
				if _, err := inputConnection.Write(fragment); err != nil {
					return
				}
			}
		}
	}()

	// Wait for the incoming connection on the output
	outputConnection, err := outputTCPServer.Accept()
	if err != nil {
		logrus.Errorf("Error reading output %s", err)
		return err
	}
	go func() {
		scanner := bufio.NewScanner(outputConnection)
		buf := make([]byte, 1<<20) // 1MB
		scanner.Buffer(buf, 1<<20)

		// Split on each JPG, {255, 217} are the last bytes of a JPG image
		scanner.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
			if atEOF && len(data) == 0 {
				return 0, nil, nil
			}

			index := bytes.Index(data, []byte{255, 217})

			if index > 0 {
				return index + 2, data[0 : index+2], nil
			}

			return 0, nil, nil
		})
		for scanner.Scan() {
			i.lastImg = scanner.Bytes()
		}
		logrus.Infof("Scanner closing")
	}()

	return nil
}

func (i *ImageExtractor) GetLastImg() ([]byte, error) {
	i.timer.Reset(i.duration)

	if !i.running && !i.starting {
		logrus.Warnf("Starting stream")
		i.starting = true
		go func() {
			if err := i.start(); err != nil {
				i.starting = false
				logrus.Errorf("Error during start %s", err.Error())
			}
		}()
	}

	if len(i.lastImg) == 0 {
		logrus.Warnf("Stream starting, fetching the snap.jpg")
		// Stream starting, fallback to the snapshot url
		return i.getSnapShot()
	}

	tmp := make([]byte, len(i.lastImg))
	copy(tmp, i.lastImg)
	return tmp, nil
}

func (i *ImageExtractor) getSnapShot() ([]byte, error) {
	resp, err := http.Get(fmt.Sprintf("http://%s/snap.jpeg", i.camera.Host))

	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	img, err := ioutil.ReadAll(resp.Body)

	if err != nil {
		return nil, err
	}
	return img, nil
}

func getRandomPort() int {
	return rand.Intn(40000) + 20000
}

func readScanner(scanner *bufio.Scanner, lines chan string) {
	for scanner.Scan() {
		lines <- scanner.Text()
	}
	close(lines)
}
