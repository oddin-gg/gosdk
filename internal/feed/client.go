package feed

import (
	"errors"
	"fmt"
	"time"

	"github.com/oddin-gg/gosdk/protocols"
	amqp "github.com/rabbitmq/amqp091-go"
	log "github.com/sirupsen/logrus"
)

// Client ...
type Client struct {
	connection            *amqp.Connection
	oddsFeedConfiguration protocols.OddsFeedConfiguration
	whoAmIManager         protocols.WhoAmIManager
	logger                *log.Logger
	closed                bool
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
	c.closed = true

	if c.connection != nil {
		_ = c.connection.Close()
	}
}

// Open ...
func (c *Client) Open() error {
	mqURL, err := c.oddsFeedConfiguration.MQURL()
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
			switch {
			case err == nil:
				continue
			case c.closed:
				return
			}

			go c.reconnect()
		}
	}()
	c.connection.NotifyClose(errorCh)

	return err
}

func (c *Client) reconnect() {
	err := c.Open()
	if err != nil {
		c.logger.WithError(err).Error("reconnect to rabbitmq failed, retrying...")
		time.Sleep(5 * time.Second)
		go c.reconnect()
	}
}
