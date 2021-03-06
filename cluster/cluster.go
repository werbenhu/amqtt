package cluster

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io/ioutil"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/werbenhu/amqtt/config"
	"github.com/werbenhu/amqtt/ifs"
	"github.com/werbenhu/amqtt/logger"
	"github.com/werbenhu/amqtt/packets"
)

type Cluster struct {
	ctx       context.Context
	cancel    context.CancelFunc
	s         ifs.Server
	processor ifs.Processor
}

func NewCluster(server ifs.Server) *Cluster {
	c := new(Cluster)
	c.s = server
	c.ctx, c.cancel = context.WithCancel(server.Context())
	c.processor = NewProcessor(server)
	return c
}

func (c *Cluster) HandlerServer(conn net.Conn) {
	client := NewClient(conn, config.TypServer)
	packet, err := client.ReadPacket()
	if err != nil {
		logger.Error("read connect packet error: ", err)
		return
	}
	cp, ok := packet.(*packets.ConnectPacket)
	if !ok {
		logger.Error("received msg that was not connect")
		return
	}

	client.SetId(cp.ClientIdentifier)
	clientId := cp.ClientIdentifier
	if old, ok := c.s.Clusters().Load(clientId); ok {
		logger.Infof("cluster HandlerServer close old clientId:%s", client.GetId())
		oldClient := old.(ifs.Client)
		oldClient.Close()
	}

	logger.Infof("cluster server receive connection from clientId:%s", client.GetId())
	c.processor.ProcessConnect(client, cp)
	client.SetId(clientId)
	c.s.Clusters().Store(clientId, client)
	c.SyncTopics(clientId)
	client.ReadLoop(c.processor)
}

func (c *Cluster) StartServer() {
	tcpHost := config.ClusterHost()
	var tcpListener net.Listener
	var err error

	if config.ClusterTls() {
		cert, err := tls.LoadX509KeyPair(config.CertFile(), config.KeyFile())
		if err != nil {
			logger.Fatalf("tcp LoadX509KeyPair ce file: %s Err:%s", config.CertFile(), err)
		}

		var ca []byte
		ca, err = ioutil.ReadFile(config.Ca())
		if err != nil {
			logger.Fatalf("cluster server unable to read root cert file")
		}
		caPool := x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(ca) {
			logger.Fatalf("cluster server add cert Pool err")
		}

		tcpListener, err = tls.Listen("tcp", tcpHost, &tls.Config{
			Certificates:       []tls.Certificate{cert},
			ClientCAs:          caPool,
			InsecureSkipVerify: false,
			ClientAuth:         tls.RequireAndVerifyClientCert,
		})

		if err != nil {
			logger.Fatalf("tsl listen to %s Err:%s", tcpHost, err)
		}
		logger.Infof("start cluster tcp listen to %s and tls is on ...", tcpHost)

	} else {
		tcpListener, err = net.Listen("tcp", tcpHost)
		if err != nil {
			logger.Fatalf("tcp listen to %s Err:%s", tcpHost, err)
		}
		logger.Infof("start cluster tcp listen to %s ...", tcpHost)
	}

	for {
		conn, err := tcpListener.Accept()
		if err != nil {
			logger.Fatalf("cluster server tcp Accept to %s Err:%s", tcpHost, err.Error())
			continue
		} else {
			go c.HandlerServer(conn)
		}
	}
}

func (c *Cluster) HandlerClient(conn net.Conn, cluster config.ClusterNode) {
	connect := packets.NewControlPacket(packets.Connect).(*packets.ConnectPacket)
	connect.ProtocolName = "MQTT"
	connect.ProtocolVersion = 4
	connect.CleanSession = true
	connect.ClientIdentifier = config.ClusterName()
	connect.Keepalive = 60

	client := NewClient(conn, config.TypClient)
	client.SetId(cluster.Name)
	err := client.WritePacket(connect)
	if err != nil {
		logger.Errorf("send cluster connect error:%s", err)
		return
	}

	if old, ok := c.s.Clusters().Load(cluster.Name); ok {
		logger.Infof("cluster HandlerClient close clientId:%s", client.GetId())
		oldClient := old.(ifs.Client)
		oldClient.Close()
	}
	client.SetId(cluster.Name)
	c.s.Clusters().Store(cluster.Name, client)
	c.SyncTopics(cluster.Name)
	client.ReadLoop(c.processor)
}

func (c *Cluster) StartClient(cluster config.ClusterNode) {
	var err error
	var conn net.Conn

	if config.ClusterTls() {
		logger.Infof("cluster client start connect to %+v with tls on...", cluster)
		var cert tls.Certificate
		cert, err = tls.LoadX509KeyPair(config.ClientCertFile(), config.ClientKeyFile())
		if err != nil {
			logger.Fatalf("cluster client LoadX509KeyPair cert file: %s Err:%s\n", config.CertFile(), err)
		}

		var ca []byte
		ca, err = ioutil.ReadFile(config.Ca())
		if err != nil {
			logger.Fatalf("cluster client unable to read root cert file")
		}
		caPool := x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(ca) {
			logger.Fatalf("cluster client add cert Pool err")
		}

		tlsConfig := &tls.Config{
			RootCAs:      caPool,
			Certificates: []tls.Certificate{cert},
			ClientAuth:   tls.NoClientCert,
			ClientCAs:    nil,
			//InsecureSkipVerify controls whether a client verifies the server's certificate chain and host name.
			InsecureSkipVerify: true,
		}
		conn, err = tls.Dial("tcp", cluster.Host, tlsConfig)

	} else {
		logger.Infof("cluster client start connect to %+v ...", cluster)
		conn, err = net.DialTimeout("tcp", cluster.Host, 60*time.Second)
	}

	if err != nil {
		logger.Errorf("Cluster fail to connect to %s Err:%s", cluster.Host, err.Error())
		return
	} else {
		c.HandlerClient(conn, cluster)
	}
}

func (c *Cluster) syncNodeTopics(wg *sync.WaitGroup, cluster config.ClusterNode) {
	clientId := strings.TrimSpace(cluster.Name)
	exist, ok := c.s.Clusters().Load(clientId)
	if ok {
		logger.Infof("syncNodeTopics to cluster:%+v", cluster)
		c.s.BrokerTopics().RangeTopics(func(topic, client interface{}) bool {
			subpack := packets.NewControlPacket(packets.Subscribe).(*packets.SubscribePacket)
			subpack.Topics = []string{topic.(string)}
			subpack.Qoss = []byte{0}
			exist.(*Client).WritePacket(subpack)
			return true
		})
	}
	wg.Done()
}

// when two cluster nodes are reconnected, topics that need to synchronize
func (c *Cluster) SyncTopics(clientId string) {
	wg := sync.WaitGroup{}
	for _, cluster := range config.Clusters() {
		if clientId == cluster.Name {
			wg.Add(1)
			func(wg *sync.WaitGroup, cluster config.ClusterNode) {
				go c.syncNodeTopics(wg, cluster)
			}(&wg, cluster)
		}
	}
	wg.Wait()
}

func (c *Cluster) CheckHealthy() {
	for _, cluster := range config.Clusters() {
		clientId := strings.TrimSpace(cluster.Name)
		exist, ok := c.s.Clusters().Load(clientId)
		if !ok {
			logger.Infof("CheckHealthy fail, connect to cluster:%+v", cluster)
			func(cluster config.ClusterNode) {
				go c.StartClient(cluster)
			}(cluster)
		} else if exist.(*Client).GetTyp() == config.TypClient {
			ping := packets.NewControlPacket(packets.Pingreq).(*packets.PingreqPacket)
			exist.(*Client).WritePacket(ping)
		}
	}
}

func (c *Cluster) HeartBeat() {
	tick := time.NewTicker(20 * time.Second)
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-tick.C:
			c.CheckHealthy()
		}
	}
}

func (c *Cluster) Start() {
	go c.StartServer()
	go c.CheckHealthy()
	go c.HeartBeat()

	<-c.ctx.Done()
	logger.Debug("cluster done")
}

func (c *Cluster) Close() {
	c.cancel()
}
