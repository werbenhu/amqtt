package cluster

import (
	"sync/atomic"

	"github.com/werbenhu/amqtt/ifs"
	"github.com/werbenhu/amqtt/logger"
	"github.com/werbenhu/amqtt/packets"
)

type Processor struct {
	s ifs.Server
}

func NewProcessor(server ifs.Server) *Processor {
	p := new(Processor)
	p.s = server
	return p
}

func (p *Processor) DoPublish(topic string, packet *packets.PublishPacket) {

	//send message to the clients in the current node that have subscribed to the topic
	subs := p.s.BrokerTopics().Subscribers(topic)
	history := make(map[string]bool)
	for _, sub := range subs {
		client := sub.(ifs.Client)
		//a message is only sent to a client once, here to remove the duplicate
		if !history[client.GetId()] {
			history[client.GetId()] = true
			client.WritePacket(packet)
			atomic.AddInt64(&p.s.State().PubSent, 1)
		}
	}
}

func (p *Processor) ProcessPublish(client ifs.Client, packet *packets.PublishPacket) {
	topic := packet.TopicName
	p.DoPublish(topic, packet)

	//if other cluster node have this retain message, then the current cluster node must delete this retain message
	//ensure that there is only one retain message for a topic in the entire clusters
	if packet.Retain {
		p.s.BrokerTopics().RemoveRetain(topic)
	}
}

func (p *Processor) ProcessUnSubscribe(client ifs.Client, packet *packets.UnsubscribePacket) {
	topics := packet.Topics
	unsuback := packets.NewControlPacket(packets.Unsuback).(*packets.UnsubackPacket)
	unsuback.MessageID = packet.MessageID
	client.WritePacket(unsuback)

	for _, topic := range topics {
		p.s.ClusterTopics().Unsubscribe(topic, client.GetId())
		client.RemoveTopic(topic)
	}
}

func (p *Processor) ProcessSubscribe(client ifs.Client, packet *packets.SubscribePacket) {
	topics := packet.Topics
	suback := packets.NewControlPacket(packets.Suback).(*packets.SubackPacket)
	suback.MessageID = packet.MessageID
	client.WritePacket(suback)

	for _, topic := range topics {
		p.s.ClusterTopics().Subscribe(topic, client.GetId(), client)
		client.AddTopic(topic, client.GetId())

		retains, _ := p.s.BrokerTopics().SearchRetain(topic)
		for _, retain := range retains {
			client.WritePacket(retain.(*packets.PublishPacket))
		}
	}
}

func (p *Processor) ProcessPing(client ifs.Client) {
	resp := packets.NewControlPacket(packets.Pingresp).(*packets.PingrespPacket)
	err := client.WritePacket(resp)
	if err != nil {
		logger.Errorf("send PingResponse error: %s\n", err)
		return
	}
}

func (p *Processor) ProcessDisconnect(client ifs.Client) {
	logger.Debugf("cluster ProcessDisconnect clientId:%s", client.GetId())
	p.s.Clusters().Delete(client.GetId())
	client.Close()

	//when a node in the cluster is disconnected, the node must unsubscript it's all topics
	for topic := range client.Topics() {
		logger.Debugf("ProcessDisconnect topic:%s", topic)
		client.RemoveTopic(topic)
		p.s.ClusterTopics().Unsubscribe(topic, client.GetId())
	}
}

func (p *Processor) ProcessConnack(client ifs.Client, cp *packets.ConnackPacket) {
	logger.Debugf("cluster ProcessConnack clientId:%s", client.GetId())
}

func (p *Processor) ProcessConnect(client ifs.Client, cp *packets.ConnectPacket) {
	connack := packets.NewControlPacket(packets.Connack).(*packets.ConnackPacket)
	connack.SessionPresent = cp.CleanSession
	connack.ReturnCode = cp.Validate()

	err := client.WritePacket(connack)
	if err != nil {
		logger.Error("send connack error, ", err)
		return
	}
}

func (p *Processor) ProcessMessage(client ifs.Client, cp packets.ControlPacket) {
	switch packet := cp.(type) {
	case *packets.ConnackPacket:
		p.ProcessConnack(client, packet)
	case *packets.ConnectPacket:
		p.ProcessConnect(client, packet)
	case *packets.PublishPacket:
		p.ProcessPublish(client, packet)
	case *packets.SubscribePacket:
		p.ProcessSubscribe(client, packet)
	case *packets.UnsubscribePacket:
		p.ProcessUnSubscribe(client, packet)
	case *packets.PingreqPacket:
		p.ProcessPing(client)
	case *packets.DisconnectPacket:
		p.ProcessDisconnect(client)

	case *packets.PubackPacket:
	case *packets.PubrecPacket:
	case *packets.PubrelPacket:
	case *packets.PubcompPacket:
	case *packets.SubackPacket:
	case *packets.UnsubackPacket:
	case *packets.PingrespPacket:
	default:
		logger.Error("Recv Unknow message")
	}
}
