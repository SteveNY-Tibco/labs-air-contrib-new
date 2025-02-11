package mqtt

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/edgexfoundry/go-mod-messaging/v2/pkg/types"

	//	"github.com/fxamacker/cbor/v2"
	"github.com/project-flogo/core/data/metadata"
	"github.com/project-flogo/core/support/log"
	"github.com/project-flogo/core/support/ssl"
	"github.com/project-flogo/core/trigger"
)

var triggerMd = trigger.NewMetadata(&Settings{}, &HandlerSettings{}, &Output{})

func init() {
	_ = trigger.Register(&Trigger{}, &Factory{})
}

// TokenType is a type of token
type TokenType int

const (
	// Literal is a literal token type
	Literal TokenType = iota
	// SingleLevel is a single level wildcard
	SingleLevel
	// MultiLevel is a multi level wildcard
	MultiLevel
)

// Token is a MQTT topic token
type Token struct {
	TokenType TokenType
	Token     string
}

// Topic is a parsed topic
type Topic []Token

// ParseTopic parses the topic
func ParseTopic(topic string) Topic {
	var parsed Topic
	parts, index := strings.Split(topic, "/"), 0
	for _, part := range parts {
		if strings.HasPrefix(part, "+") {
			token := strings.TrimPrefix(part, "+")
			if token == "" {
				token = strconv.Itoa(index)
				index++
			}
			parsed = append(parsed, Token{
				TokenType: SingleLevel,
				Token:     token,
			})
		} else if strings.HasPrefix(part, "#") {
			token := strings.TrimPrefix(part, "#")
			if token == "" {
				token = strconv.Itoa(index)
				index++
			}
			parsed = append(parsed, Token{
				TokenType: MultiLevel,
				Token:     token,
			})
		} else {
			parsed = append(parsed, Token{
				TokenType: Literal,
				Token:     part,
			})
		}
	}
	return parsed
}

// Match matches the topic with an input topic
func (t Topic) Match(input Topic) map[string]string {
	output, i := make(map[string]string), 0
MATCHER:
	for _, token := range t {
		switch token.TokenType {
		case Literal:
			if i > len(input) {
				break MATCHER
			}
			if token.Token != input[i].Token {
				break MATCHER
			}
		case SingleLevel:
			if i >= len(input) {
				output[token.Token] = ""
				break MATCHER
			}
			output[token.Token] = input[i].Token
		case MultiLevel:
			if i >= len(input) {
				output[token.Token] = ""
				break MATCHER
			}
			levels, sep := "", ""
			for _, level := range input[i:] {
				levels += sep + level.Token
				sep = "/"
				i++
			}
			output[token.Token] = levels
			break MATCHER
		}
		i++
	}
	return output
}

// String generates a string for the topic
func (t Topic) String() string {
	output := strings.Builder{}
	for i, token := range t {
		if i > 0 {
			output.WriteString("/")
		}
		switch token.TokenType {
		case Literal:
			output.WriteString(token.Token)
		case SingleLevel:
			output.WriteString("+")
		case MultiLevel:
			output.WriteString("#")
		}
	}
	return output.String()
}

// Trigger is simple MQTT trigger
type Trigger struct {
	handlers map[string]*clientHandler
	settings *Settings
	logger   log.Logger
	options  *mqtt.ClientOptions
	client   mqtt.Client
}
type clientHandler struct {
	//client mqtt.Client
	handler  trigger.Handler
	settings *HandlerSettings
}
type Factory struct {
}

func (*Factory) Metadata() *trigger.Metadata {
	return triggerMd
}

// New implements trigger.Factory.New
func (*Factory) New(config *trigger.Config) (trigger.Trigger, error) {
	s := &Settings{}

	err := metadata.MapToStruct(config.Settings, s, true)
	if err != nil {
		return nil, err
	}

	return &Trigger{settings: s}, nil
}

// Initialize implements trigger.Initializable.Initialize
func (t *Trigger) Initialize(ctx trigger.InitContext) error {
	t.logger = ctx.Logger()

	settings := t.settings

	t.logger.Debug("Recieving SETTINGS : ", settings)

	options := t.initClientOption(settings)
	t.options = options

	if strings.HasPrefix(settings.Broker, "ssl") {
		cfg := &ssl.Config{}

		if len(settings.SSLConfig) != 0 {
			err := cfg.FromMap(settings.SSLConfig)
			if err != nil {
				return err
			}

			if _, set := settings.SSLConfig["skipVerify"]; !set {
				cfg.SkipVerify = true
			}
			if _, set := settings.SSLConfig["useSystemCert"]; !set {
				cfg.UseSystemCert = true
			}
		} else {
			//using ssl but not configured, use defaults
			cfg.SkipVerify = true
			cfg.UseSystemCert = true
		}

		tlsConfig, err := ssl.NewClientTLSConfig(cfg)
		if err != nil {
			return err
		}

		options.SetTLSConfig(tlsConfig)
	}

	options.SetDefaultPublishHandler(func(client mqtt.Client, msg mqtt.Message) {
		t.logger.Warnf("Recieved message on unhandled topic: %s", msg.Topic())
	})

	//t.logger.Debugf("Client options: %v", options)

	t.handlers = make(map[string]*clientHandler)

	for _, handler := range ctx.GetHandlers() {

		s := &HandlerSettings{}
		err := metadata.MapToStruct(handler.Settings(), s, true)
		if err != nil {
			return err
		}

		t.handlers[s.Topic] = &clientHandler{handler: handler, settings: s}
	}

	return nil
}

func (t *Trigger) initClientOption(settings *Settings) *mqtt.ClientOptions {

	opts := mqtt.NewClientOptions()
	opts.AddBroker(settings.Broker)
	opts.SetClientID(settings.Id)
	opts.SetUsername(settings.Username)
	password := settings.Password
	if strings.HasPrefix(password, "SECRET:") {
		pwdBytes, _ := base64.StdEncoding.DecodeString(password[7:])
		password = string(pwdBytes)
	}
	opts.SetPassword(password)
	opts.SetCleanSession(settings.CleanSession)
	opts.SetAutoReconnect(settings.AutoReconnect)

	if settings.Store != ":memory:" && settings.Store != "" {
		opts.SetStore(mqtt.NewFileStore(settings.Store))
	}

	if settings.KeepAlive != 0 {
		opts.SetKeepAlive(time.Duration(settings.KeepAlive) * time.Second)
	} else {
		opts.SetKeepAlive(2 * time.Second)
	}

	opts.SetOnConnectHandler(func(client mqtt.Client) {
		t.logger.Debugf("OnConnectHandler get called: client = %v", client)
	})
	opts.SetConnectionLostHandler(func(client mqtt.Client, err error) {
		t.logger.Debugf("ConnectionLostHandler get called: client = %v", client, ", err = %s", err.Error())
	})
	opts.SetReconnectingHandler(func(client mqtt.Client, opts *mqtt.ClientOptions) {
		t.logger.Debugf("ReconnectingHandler get called: client = %v", client, ", opts = %v", opts)
	})
	opts.SetConnectionAttemptHandler(func(broker *url.URL, tlsCfg *tls.Config) *tls.Config {
		t.logger.Debugf("ConnectionAttemptHandler get called: broker = %v", broker)
		return nil
	})

	return opts
}

// Start implements trigger.Trigger.Start
func (t *Trigger) Start() error {

	client := mqtt.NewClient(t.options)

	if token := client.Connect(); token.Wait() && token.Error() != nil {
		return token.Error()
	}

	t.client = client

	for _, handler := range t.handlers {
		parsed := ParseTopic(handler.settings.Topic)
		if token := client.Subscribe(parsed.String(), byte(handler.settings.Qos), t.getHanlder(handler, parsed)); token.Wait() && token.Error() != nil {
			t.logger.Errorf("Error subscribing to topic %s: %s", handler.settings.Topic, token.Error())
			return token.Error()
		}

		t.logger.Debugf("Subscribed to topic: %s", handler.settings.Topic)
	}

	return nil
}

// Stop implements ext.Trigger.Stop
func (t *Trigger) Stop() error {

	//unsubscribe from topics
	for _, handler := range t.handlers {
		topic := ParseTopic(handler.settings.Topic).String()
		t.logger.Debug("Unsubscribing from topic: ", topic)
		if token := t.client.Unsubscribe(topic); token.Wait() && token.Error() != nil {
			t.logger.Errorf("Error unsubscribing from topic %s: %s", topic, token.Error())
		}
	}

	t.client.Disconnect(250)

	return nil
}

func (t *Trigger) getHanlder(handler *clientHandler, parsed Topic) func(mqtt.Client, mqtt.Message) {
	return func(client mqtt.Client, msg mqtt.Message) {
		topic := msg.Topic()
		qos := msg.Qos()
		payload := msg.Payload()
		params := parsed.Match(ParseTopic(topic))

		t.logger.Debugf("Topic[%s] - Payload Recieved: %s", topic, string(payload))

		result, err := t.runHandler(handler.handler, payload, topic, params, handler.settings.Deserializer)
		if err != nil {
			t.logger.Error("Error handling message: %v", err)
			return
		}

		if handler.settings.ReplyTopic != "" {
			reply := &Reply{}
			err = reply.FromMap(result)
			if err != nil {
				t.logger.Error("Error handling message: %v", err)
				return
			}

			if reply.Data != nil {
				dataJson, err := json.Marshal(reply.Data)
				if err != nil {
					return
				}
				token := client.Publish(handler.settings.ReplyTopic, qos, handler.settings.Retain, string(dataJson))
				sent := token.WaitTimeout(5000 * time.Millisecond)
				if !sent {
					t.logger.Errorf("Timeout occurred while trying to publish reply to topic '%s'", handler.settings.ReplyTopic)
					return
				}
			}
		}
	}
}

// RunHandler runs the handler and associated action
func (t *Trigger) runHandler(handler trigger.Handler, payload []byte, topic string, params map[string]string, deserializer string) (map[string]interface{}, error) {
	var content interface{}
	switch deserializer {
	case "JSON":
		err := json.Unmarshal(payload, &content)
		if err != nil {
			return nil, err
		}
	case "EDGEX_JSON":
		edgexEnvelope := types.MessageEnvelope{}
		err := json.Unmarshal(payload, &edgexEnvelope)
		if err != nil {
			return nil, err
		}
		content = map[string]interface{}{
			"ContentType":   edgexEnvelope.ContentType,
			"CorrelationID": edgexEnvelope.CorrelationID,
			"Payload":       edgexEnvelope.Payload,
			"ReceivedTopic": edgexEnvelope.ReceivedTopic,
		}
	case "Base64":
		byteContent, err := base64.StdEncoding.DecodeString(string(payload))
		if err != nil {
			return nil, err
		}
		content = string(byteContent)
	case "Base64_JSON":
		decodedByteContent, err := base64.StdEncoding.DecodeString(string(payload))
		if err != nil {
			return nil, err
		}
		err = json.Unmarshal(decodedByteContent, &content)
		if err != nil {
			return nil, err
		}
	default:
		content = payload
	}

	topicParams := make(map[string]interface{})
	for key, value := range params {
		topicParams[key] = value
	}

	out := &Output{
		Id:          t.settings.Id,
		Content:     content,
		Topic:       topic,
		TopicParams: topicParams,
	}

	results, err := handler.Handle(context.Background(), out)
	if err != nil {
		return nil, err
	}

	return results, nil
}
