package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/XciD/unifi-protect/pkg/protect"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

var (
	hooks = map[string]Hook{}
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
}

type Hook func(message *protect.WsMessage)

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

	events, err := protect.NewWebsocketEvent(nvr)
	if err != nil {
		panic(err)
	}

	url := viper.GetString("hook.url")
	user := viper.GetString("hook.user")
	password := viper.GetString("hook.password")

	addHook("camera", viper.GetString("camera.id"), "update", func(m *protect.WsMessage) {
		update := protect.ProtectNvrUpdatePayloadCameraUpdate{}
		if err := m.Payload.GetJSON(&update); err != nil {
			logrus.Warningf("Error during unmarshal %s", err)
			return
		}

		if update.LastRing != 0 {
			logrus.Infof("lastRing received %d", update.LastRing)
			date := time.Unix(int64(update.LastRing), 0)

			if date.Add(1 * time.Minute).After(time.Now()) {
				logrus.Infof("lastRing seems to be now triggering")
				SendHook(url, user, password)
			}
		}
	})

	for {
		select {
		case message := <-events.Events:
			action, err := message.GetAction()

			if err != nil {
				logrus.Warningf("Skipping message due to err: %s", err)
				continue
			}

			if h, ok := hooks[fmt.Sprintf("%s:%s:%s", action.ModelKey, action.ID, action.Action)]; ok {
				h(message)
			}
		}
	}
}

func addHook(camera, id, action string, hook Hook) {
	hooks[fmt.Sprintf("%s:%s:%s", camera, id, action)] = hook
}

func SendHook(url, user, password string) {
	request, _ := http.NewRequest(http.MethodGet, url, nil)
	request.SetBasicAuth(user, password)
	http.DefaultClient.Do(request)
}
