package http

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/tinode/chat/server/drafty"
	"github.com/tinode/chat/server/store"

	"log"
	"net/http"

	t "github.com/tinode/chat/server/store/types"

	"github.com/tinode/chat/server/push"
)

var handler httpPush

// How much to buffer the input channel.
const defaultBuffer = 32

type httpPush struct {
	initialized bool
	input       chan *push.Receipt
	channel     chan *push.ChannelReq // note: not implemented yet
	stop        chan bool
}

type configType struct {
	Enabled bool   `json:"enabled"`
	Buffer  int    `json:"buffer"`
	Url     string `json:"url"`
}

// Init initializes the handler
func (httpPush) Init(jsonconf string) error {
	log.Printf("Init HTTP push")

	// Check if the handler is already initialized
	if handler.initialized {
		return errors.New("already initialized")
	}

	var config configType
	if err := json.Unmarshal([]byte(jsonconf), &config); err != nil {
		return errors.New("failed to parse config: " + err.Error())
	}

	handler.initialized = true

	if !config.Enabled {
		return nil
	}

	if config.Buffer <= 0 {
		config.Buffer = defaultBuffer
	}

	handler.input = make(chan *push.Receipt, config.Buffer)
	handler.stop = make(chan bool, 1)

	go func() {
		for {
			select {
			case msg := <-handler.input:
				go sendPushToHttp(msg, config.Url)
			case <-handler.stop:
				return
			}
		}
	}()

	log.Printf("Initialized HTTP push")
	return nil
}

func messagePayload(payload *push.Payload) map[string]string {
	data := make(map[string]string)
	data["topic"] = payload.Topic
	data["silent"] = strconv.FormatBool(payload.Silent)
	data["from"] = payload.From
	data["ts"] = payload.Timestamp.Format(time.RFC3339)
	data["seq"] = strconv.Itoa(payload.SeqId)
	data["mime"] = payload.ContentType
	data["content"], _ = drafty.PlainText(payload.Content)

	return data
}

func sendPushToHttp(msg *push.Receipt, url string) {
	log.Println("Prepare to sent HTTP push from: ", msg.Payload.From)
	msgM, _ := json.Marshal(msg)
	log.Println("Push Message", string(msgM))

	recipientsIds := make([]t.Uid, len(msg.To))
	for recipientId := range msg.To {
		recipientsIds = append(recipientsIds, recipientId)
	}

	/*
	* Sender user data
	 */
	sender, _ := store.Users.Get(t.ParseUserId(msg.Payload.From))
	log.Println("notification topic id: ", msg.Payload.Topic)
	topic, _ := store.Topics.Get(msg.Payload.Topic)
	log.Println("notification topic: ", topic)

	/*
	* Recipients list with user data, and conversation status
	 */
	recipientsList, _ := store.Users.GetAll(recipientsIds...)
	recipients := map[string]map[string]interface{}{}
	for _, r := range recipientsList {
		user := map[string]interface{}{
			"user": r,
		}
		recipients[r.Id] = user
	}
	for uid, to := range msg.To {
		recipients[uid.String()]["device"] = to
	}

	/*
	* Generate payload
	 */
	data := make(map[string]interface{})
	data["recipients"] = recipients
	data["sender"] = sender
	data["topic"] = topic
	data["organizationId"] = msg.OrganizationId
	data["payload"] = messagePayload(&msg.Payload)
	data["head"] = msg.Payload.Head
	data["what"] = msg.Payload.What
	requestData, _ := json.Marshal(data)

	/*
	* Send push through http
	 */
	log.Println("Sent HTTP push from: ", sender.Id, "to: ", recipientsIds)
	_, err := http.Post(url, "application/json", bytes.NewBuffer(requestData))
	if err != nil {
		log.Println("Http send push failed: ", err)
	}
}

// IsReady checks if the handler is initialized.
func (httpPush) IsReady() bool {
	return handler.input != nil
}

// Push returns a channel that the server will use to send messages to.
// If the adapter blocks, the message will be dropped.
func (httpPush) Push() chan<- *push.Receipt {
	return handler.input
}

// Channel returns a channel for subscribing/unsubscribing devices to FCM topics.
func (httpPush) Channel() chan<- *push.ChannelReq {
	return handler.channel
}

// Stop terminates the handler's worker and stops sending pushes.
func (httpPush) Stop() {
	handler.stop <- true
}

func init() {
	push.Register("http", &handler)
}
