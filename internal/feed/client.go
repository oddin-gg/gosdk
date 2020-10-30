package feed

import (
	"errors"
	"fmt"
	"github.com/oddin-gg/gosdk/protocols"
	log "github.com/sirupsen/logrus"
	"github.com/streadway/amqp"
	"sync"
	"time"
)

// Client ...
type Client struct {
	connection            *amqp.Connection
	oddsFeedConfiguration protocols.OddsFeedConfiguration
	whoAmIManager         protocols.WhoAmIManager
	mux                   sync.Mutex
	errorCh               chan *amqp.Error
	logger                *log.Logger
}

// NewClient ...
func NewClient(oddsFeedConfiguration protocols.OddsFeedConfiguration, whoAmIManager protocols.WhoAmIManager, logger *log.Logger) *Client {
	return &Client{
		oddsFeedConfiguration: oddsFeedConfiguration,
		whoAmIManager:         whoAmIManager,
		logger:                logger,
	}
}

// CreateChannel ...
func (c *Client) CreateChannel(routingKeys []string, exchangeName string) (<-chan amqp.Delivery, error) {
	if c.connection == nil {
		return nil, errors.New("connection is not opened")
	}

	channel, err := c.connection.Channel()
	if err != nil {
		return nil, err
	}

	queue, err := channel.QueueDeclare(
		"",    // name
		false, // durable
		true,  // delete when unused
		true,  // exclusive
		false, // no-wait
		nil,   // arguments
	)
	if err != nil {
		return nil, err
	}

	for _, routingKey := range routingKeys {
		err = channel.QueueBind(
			queue.Name,   // queue name
			routingKey,   // routing key
			exchangeName, // exchange
			false,
			nil)
		if err != nil {
			return nil, err
		}
	}

	return channel.Consume(
		queue.Name,
		"",
		true,
		true,
		false,
		false,
		nil)
}

// Close ...
func (c *Client) Close() {
	_ = c.connection.Close()
}

// Open ...
func (c *Client) Open() error {
	mqURL, err := c.oddsFeedConfiguration.SelectedEnvironment().MQEndpoint()
	if err != nil {
		return err
	}

	details, err := c.whoAmIManager.BookmakerDetails()
	if err != nil {
		return err
	}

	vHost := fmt.Sprintf("/oddinfeed/%d", details.BookmakerID())
	amqpURL := fmt.Sprintf(
		"amqps://%s:%s@%s:%d",
		*c.oddsFeedConfiguration.AccessToken(),
		"",
		mqURL,
		c.oddsFeedConfiguration.MessagingPort())

	properties := make(map[string]interface{})
	properties["SDK"] = "go"

	c.connection, err = amqp.DialConfig(amqpURL,
		amqp.Config{
			Vhost:      vHost,
			Properties: properties,
		},
	)
	if err != nil {
		return err
	}

	errorCh := make(chan *amqp.Error, 1)
	go func() {
		for err := range errorCh {
			if err == nil {
				continue
			}

			go c.reconnect()
		}
	}()
	c.connection.NotifyClose(errorCh)

	return err
}

func (c *Client) reconnect() {
	c.Close()

	err := c.Open()
	if err != nil {
		c.logger.WithError(err).Error("reconnect to rabbitmq failed, retrying...")
		time.Sleep(5 * time.Second)
		go c.reconnect()
	}
}
