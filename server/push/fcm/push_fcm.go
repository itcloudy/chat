// Package fcm implements push notification plugin for Google FCM backend.
// Push notifications for Android, iOS and web clients are sent through Google's Firebase Cloud Messaging service.
package fcm

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strconv"
	"time"

	fbase "firebase.google.com/go"
	fcm "firebase.google.com/go/messaging"

	"github.com/tinode/chat/server/drafty"
	"github.com/tinode/chat/server/push"
	"github.com/tinode/chat/server/store"
	t "github.com/tinode/chat/server/store/types"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
)

var handler Handler

// Size of the input channel buffer.
const defaultBuffer = 32

// Maximum length of a text message in runes
const maxMessageLength = 80

// Handler represents the push handler; implements push.PushHandler interface.
type Handler struct {
	input  chan *push.Receipt
	stop   chan bool
	client *fcm.Client
}

// Configuration of AndroidNotification payload.
type androidConfig struct {
	Enabled bool `json:"enabled,omitempty"`
	// Common defauls for all push types.
	androidPayload
	// Configs for specific push types.
	Msg androidPayload `json:"msg,omitempty"`
	Sub androidPayload `json:"msg,omitempty"`
}

func (ac *androidConfig) getTitleLocKey(what string) string {
	var title string
	if what == push.ActMsg {
		title = ac.Msg.TitleLocKey
	} else if what == push.ActSub {
		title = ac.Sub.TitleLocKey
	}
	if title == "" {
		title = ac.androidPayload.TitleLocKey
	}
	return title
}

func (ac *androidConfig) getTitle(what string) string {
	var title string
	if what == push.ActMsg {
		title = ac.Msg.Title
	} else if what == push.ActSub {
		title = ac.Sub.Title
	}
	if title == "" {
		title = ac.androidPayload.Title
	}
	return title
}

func (ac *androidConfig) getBodyLocKey(what string) string {
	var body string
	if what == push.ActMsg {
		body = ac.Msg.BodyLocKey
	} else if what == push.ActSub {
		body = ac.Sub.BodyLocKey
	}
	if body == "" {
		body = ac.androidPayload.BodyLocKey
	}
	return body
}

func (ac *androidConfig) getBody(what string) string {
	var body string
	if what == push.ActMsg {
		body = ac.Msg.Body
	} else if what == push.ActSub {
		body = ac.Sub.Body
	}
	if body == "" {
		body = ac.androidPayload.Body
	}
	return body
}

func (ac *androidConfig) getIcon(what string) string {
	var icon string
	if what == push.ActMsg {
		icon = ac.Msg.Icon
	} else if what == push.ActSub {
		icon = ac.Sub.Icon
	}
	if icon == "" {
		icon = ac.androidPayload.Icon
	}
	return icon
}

func (ac *androidConfig) getIconColor(what string) string {
	var color string
	if what == push.ActMsg {
		color = ac.Msg.IconColor
	} else if what == push.ActSub {
		color = ac.Sub.IconColor
	}
	if color == "" {
		color = ac.androidPayload.IconColor
	}
	return color
}

// Payload to be sent for a specific notification type.
type androidPayload struct {
	TitleLocKey string `json:"title_loc_key,omitempty"`
	Title       string `json:"title,omitempty"`
	BodyLocKey  string `json:"body_loc_key,omitempty"`
	Body        string `json:"body,omitempty"`
	Icon        string `json:"icon,omitempty"`
	IconColor   string `json:"icon_color,omitempty"`
	ClickAction string `json:"click_action,omitempty"`
}

type configType struct {
	Enabled         bool            `json:"enabled"`
	Buffer          int             `json:"buffer"`
	Credentials     json.RawMessage `json:"credentials"`
	CredentialsFile string          `json:"credentials_file"`
	TimeToLive      uint            `json:"time_to_live,omitempty"`
	Android         androidConfig   `json:"android,omitempty"`
}

// Init initializes the push handler
func (Handler) Init(jsonconf string) error {

	var config configType
	err := json.Unmarshal([]byte(jsonconf), &config)
	if err != nil {
		return errors.New("failed to parse config: " + err.Error())
	}

	if !config.Enabled {
		return nil
	}
	ctx := context.Background()

	var opt option.ClientOption
	if config.Credentials != nil {
		credentials, err := google.CredentialsFromJSON(ctx, config.Credentials,
			"https://www.googleapis.com/auth/firebase.messaging")
		if err != nil {
			return err
		}
		opt = option.WithCredentials(credentials)
	} else if config.CredentialsFile != "" {
		opt = option.WithCredentialsFile(config.CredentialsFile)
	} else {
		return errors.New("missing credentials")
	}

	app, err := fbase.NewApp(ctx, &fbase.Config{}, opt)
	if err != nil {
		return err
	}

	handler.client, err = app.Messaging(ctx)
	if err != nil {
		return err
	}

	if config.Buffer <= 0 {
		config.Buffer = defaultBuffer
	}

	handler.input = make(chan *push.Receipt, config.Buffer)
	handler.stop = make(chan bool, 1)

	go func() {
		for {
			select {
			case rcpt := <-handler.input:
				go sendNotifications(rcpt, &config)
			case <-handler.stop:
				return
			}
		}
	}()

	return nil
}

func sendNotifications(rcpt *push.Receipt, config *configType) {
	ctx := context.Background()

	data, _ := payloadToData(&rcpt.Payload)
	if data == nil {
		log.Println("fcm push: could not parse payload")
		return
	}

	// List of UIDs for querying the database
	uids := make([]t.Uid, len(rcpt.To))
	skipDevices := make(map[string]bool)
	i := 0
	for uid, to := range rcpt.To {
		uids[i] = uid
		i++

		// Some devices were online and received the message. Skip them.
		for _, deviceID := range to.Devices {
			skipDevices[deviceID] = true
		}
	}

	devices, count, err := store.Devices.GetAll(uids...)
	if err != nil {
		log.Println("fcm push: db error", err)
		return
	}
	if count == 0 {
		return
	}

	var titlelc, title, bodylc, body, icon, color string
	if config.Android.Enabled {
		titlelc = config.Android.getTitleLocKey(rcpt.Payload.What)
		title = config.Android.getTitle(rcpt.Payload.What)
		bodylc = config.Android.getBodyLocKey(rcpt.Payload.What)
		body = config.Android.getBody(rcpt.Payload.What)
		if body == "$content" {
			body = data["content"]
		}
		icon = config.Android.getIcon(rcpt.Payload.What)
		color = config.Android.getIconColor(rcpt.Payload.What)
	}

	for uid, devList := range devices {
		for i := range devList {
			d := &devList[i]
			if _, ok := skipDevices[d.DeviceId]; !ok && d.DeviceId != "" {
				msg := fcm.Message{
					Token: d.DeviceId,
					Data:  data,
				}

				if d.Platform == "android" {
					msg.Android = &fcm.AndroidConfig{
						Priority: "high",
					}
					if config.Android.Enabled {
						// When this notification type is included and the app is not in the foreground
						// Android won't wake up the app and won't call FirebaseMessagingService:onMessageReceived.
						// See dicussion: https://github.com/firebase/quickstart-js/issues/71
						msg.Android.Notification = &fcm.AndroidNotification{
							// Android uses Tag value to group notifications together:
							// show just one notification per topic.
							Tag:         rcpt.Payload.Topic,
							TitleLocKey: titlelc,
							Title:       title,
							BodyLocKey:  bodylc,
							Body:        body,
							Icon:        icon,
							Color:       color,
						}
					}
				} else if d.Platform == "ios" {
					// iOS uses Badge to show the total unread message count.
					badge := rcpt.To[uid].Unread
					// Need to duplicate these in APNS.Payload.Aps.Alert so
					// iOS may call NotificationServiceExtension (if present).
					title := "New message"
					body := data["content"]
					msg.APNS = &fcm.APNSConfig{
						Payload: &fcm.APNSPayload{
							Aps: &fcm.Aps{
								Badge:            &badge,
								ContentAvailable: true,
								MutableContent:   true,
								Sound:            "default",
								Alert: &fcm.ApsAlert{
									Title: title,
									Body:  body,
								},
							},
						},
					}
					msg.Notification = &fcm.Notification{
						Title: title,
						Body:  body,
					}
				}

				_, err := handler.client.Send(ctx, &msg)
				if err != nil {
					if fcm.IsMessageRateExceeded(err) ||
						fcm.IsServerUnavailable(err) ||
						fcm.IsInternal(err) ||
						fcm.IsUnknown(err) {
						// Transient errors. Stop sending this batch.
						log.Println("fcm transient failure", err)
						return
					}

					if fcm.IsMismatchedCredential(err) || fcm.IsInvalidArgument(err) {
						// Config errors
						log.Println("fcm push: failed", err)
						return
					}

					if fcm.IsRegistrationTokenNotRegistered(err) {
						// Token is no longer valid.
						log.Println("fcm push: invalid token", err)
						err = store.Devices.Delete(uid, d.DeviceId)
						if err != nil {
							log.Println("fcm push: failed to delete invalid token", err)
						}
					} else {
						log.Println("fcm push:", err)
					}
				}
			}
		}
	}
}

func payloadToData(pl *push.Payload) (map[string]string, error) {
	if pl == nil {
		return nil, nil
	}

	data := make(map[string]string)
	var err error
	data["what"] = pl.What
	if pl.Silent {
		data["silent"] = "true"
	}
	data["topic"] = pl.Topic
	data["ts"] = pl.Timestamp.Format(time.RFC3339Nano)
	// Must use "xfrom" because "from" is a reserved word. Google did not bother to document it anywhere.
	data["xfrom"] = pl.From
	if pl.What == push.ActMsg {
		data["seq"] = strconv.Itoa(pl.SeqId)
		data["mime"] = pl.ContentType
		data["content"], err = drafty.ToPlainText(pl.Content)
		if err != nil {
			return nil, err
		}

		// Trim long strings to 80 runes.
		// Check byte length first and don't waste time converting short strings.
		if len(data["content"]) > maxMessageLength {
			runes := []rune(data["content"])
			if len(runes) > maxMessageLength {
				data["content"] = string(runes[:maxMessageLength]) + "…"
			}
		}
	} else if pl.What == push.ActSub {
		data["modeWant"] = pl.ModeWant.String()
		data["modeGiven"] = pl.ModeGiven.String()
	} else {
		return nil, errors.New("unknown push type")
	}
	return data, nil
}

// IsReady checks if the push handler has been initialized.
func (Handler) IsReady() bool {
	return handler.input != nil
}

// Push returns a channel that the server will use to send messages to.
// If the adapter blocks, the message will be dropped.
func (Handler) Push() chan<- *push.Receipt {
	return handler.input
}

// Stop shuts down the handler
func (Handler) Stop() {
	handler.stop <- true
}

func init() {
	push.Register("fcm", &handler)
}
