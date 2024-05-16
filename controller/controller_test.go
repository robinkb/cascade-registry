package controller

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/distribution/distribution/v3/configuration"
	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// I swear that this is the easiest way to do it.
const registryConf = `
version: 0.1
storage:
  nats: {}
`

func TestClusterFormation(t *testing.T) {
	rgc, err := configuration.Parse(bytes.NewBufferString(registryConf))
	if err != nil {
		t.Error(err)
	}

	dc := NewDiscoveryClient()
	controllers := []*controller{}

	// Initialize the controllers
	for i := 0; i < 3; i++ {
		controllers = append(controllers, NewController(dc, &server.Options{
			JetStream:  true,
			StoreDir:   t.TempDir(),
			Port:       -1,
			ServerName: fmt.Sprintf("n%d", i),
			Cluster: server.ClusterOpts{
				Name: "cascade",
				Host: "localhost",
				Port: 6222 + i,
			},
		}, rgc))
	}

	// Start all of them
	for _, c := range controllers {
		t.Logf("starting %s", c.nso.ServerName)
		c.Run()
	}

	// Wait for all NATS servers to have started.
	// Maybe this should be a StatusNATS call on the controller.
	for _, c := range controllers {
		for {
			if c.ns == nil {
				time.Sleep(100 * time.Millisecond)
				continue
			}

			if !c.ns.ReadyForConnections(4 * time.Second) {
				continue
			}

			break
		}
	}

	// Check if all of them are clustered. Not sure if this is a good check.
	for _, c := range controllers {
		if !c.ns.JetStreamIsClustered() {
			t.Error("not clustered")
		}
	}

	// Shut it all down.
	for _, c := range controllers {
		c.Shutdown()
		c.WaitForShutdown()
	}

	t.Log("shutdown complete")
}

// 1. Start virtual node with no tags
// 2. Start actual node tagged with "cascade" or something
// 3. Any created streams must select the "cascade" tag for placement
// 4. Second real node joins the cluster
// 5. Virtual node is removed

// This works!! 🎉
func TestClusterBootstrap(t *testing.T) {
	dc := NewDiscoveryClient()

	virtualOpts := makeNATSTestOptions(t, 0)
	virtualOpts.Tags = nil // Virtual server should be untagged.
	dc.Set(virtualOpts.ServerName, makeRouteURL(virtualOpts))

	server1Opts := makeNATSTestOptions(t, 1)
	dc.Set(server1Opts.ServerName, makeRouteURL(server1Opts))

	virtualOpts.Routes = dc.Routes()
	server1Opts.Routes = dc.Routes()

	virtual, err := server.NewServer(virtualOpts)
	if err != nil {
		t.Fatal(err)
	}
	virtual.ConfigureLogger()
	virtual.Start()

	server1, err := server.NewServer(server1Opts)
	if err != nil {
		t.Fatal(err)
	}
	server1.ConfigureLogger()
	server1.Start()

	if !virtual.ReadyForConnections(10 * time.Second) {
		t.Fatal("server1 not ready")
	}

	if !server1.ReadyForConnections(10 * time.Second) {
		t.Fatal("server1 not ready")
	}

	if !server1.JetStreamIsClustered() {
		t.Fatal("server1 is not clustered")
	}

	// How can I check if the servers are _really_ ready?
	time.Sleep(8 * time.Second)

	nc1, err := nats.Connect(server1.ClientURL())
	if err != nil {
		t.Fatal(err)
	}

	js1, err := jetstream.New(nc1)
	if err != nil {
		t.Fatal(err)
	}

	// TODO: Also put some objects in here.
	_, err = js1.CreateObjectStore(context.Background(), jetstream.ObjectStoreConfig{
		Bucket:   "testing",
		Replicas: 1,
		Placement: &jetstream.Placement{
			Tags: []string{"app:cascade"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	server2Opts := makeNATSTestOptions(t, 2)
	dc.Set(server2Opts.ServerName, makeRouteURL(server2Opts))

	server2Opts.Routes = dc.Routes()

	server2, err := server.NewServer(server2Opts)
	if err != nil {
		t.Fatal(err)
	}
	server2.ConfigureLogger()
	server2.Start()

	time.Sleep(8 * time.Second)
	dc.Delete(virtualOpts.ServerName)
	if err := virtual.DisableJetStream(); err != nil {
		t.Fatal(err)
	}

	server1Opts.Routes = dc.Routes()
	server2Opts.Routes = dc.Routes()

	if err := server1.ReloadOptions(server1Opts); err != nil {
		t.Fatal(err)
	}
	if err := server2.ReloadOptions(server2Opts); err != nil {
		t.Fatal(err)
	}

	virtual.Shutdown()
	virtual.WaitForShutdown()

	_, err = js1.UpdateObjectStore(context.Background(), jetstream.ObjectStoreConfig{
		Bucket:   "testing",
		Replicas: 2,
		Placement: &jetstream.Placement{
			Tags: []string{"app:cascade"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	server3Opts := makeNATSTestOptions(t, 3)
	dc.Set(server3Opts.ServerName, makeRouteURL(server3Opts))

	server3Opts.Routes = dc.Routes()

	server3, err := server.NewServer(server3Opts)
	if err != nil {
		t.Fatal(err)
	}
	server3.ConfigureLogger()

	if err := server.Run(server3); err != nil {
		t.Fatal(err)
	}

	server1Opts.Routes = dc.Routes()
	server2Opts.Routes = dc.Routes()

	if err := server1.ReloadOptions(server1Opts); err != nil {
		t.Fatal(err)
	}
	if err := server2.ReloadOptions(server2Opts); err != nil {
		t.Fatal(err)
	}

	time.Sleep(8 * time.Second)

	_, err = js1.UpdateObjectStore(context.Background(), jetstream.ObjectStoreConfig{
		Bucket:   "testing",
		Replicas: 3,
		Placement: &jetstream.Placement{
			Tags: []string{"app:cascade"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(10 * time.Second)

}

// makeNATSTestOptions returns NATS Server options suitable for testing.
func makeNATSTestOptions(t *testing.T, index int) *server.Options {
	return &server.Options{
		ServerName: fmt.Sprintf("s%d", index),
		Port:       4222 + index,
		JetStream:  true,
		StoreDir:   t.TempDir(),
		Tags:       jwt.TagList{"app:cascade"},
		Cluster: server.ClusterOpts{
			Name: "cascade",
			Host: "localhost",
			Port: 6222 + index,
		},
		// SystemAccount: "$SYS",
		// Accounts: []*server.Account{
		// 	{
		// 		Name: "$SYS",
		// 	},
		// },
		// Users: []*server.User{
		// 	{
		// 		Username: "admin",
		// 		Password: "admin",
		// 		Account:  &server.Account{Name: "$SYS"},
		// 	},
		// },
	}
}

func makeRouteURL(opts *server.Options) *url.URL {
	return &url.URL{
		Host: fmt.Sprintf(
			"%s:%d",
			opts.Cluster.Host,
			opts.Cluster.Port,
		),
	}
}
