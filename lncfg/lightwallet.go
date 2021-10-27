package lncfg

// lightWalletConfig holds the configuration options for the daemon's connection to
// Stakenet's lightwallet.
type LightWallet struct {
	Dir             string `long:"dir" description:"The base directory that contains the node's data, logs, configuration file, etc."`
	RPCHost         string `long:"rpchost" description:"The daemon's rpc listening address. If a port is omitted, then the default port for the selected chain parameters will be used."`
	RPCUser         string `long:"rpcuser" description:"Username for RPC connections"`
	RPCPass         string `long:"rpcpass" default-mask:"-" description:"Password for RPC connections"`
	ZMQPubRawHeader string `long:"zmqpubrawheader" description:"The address listening for ZMQ connections to deliver raw header notifications"`
	UseWalletBackend bool  `long:"usewalletbackend" description:"Use light wallet as lnwallet backend"`
}
