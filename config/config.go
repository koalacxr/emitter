/**********************************************************************************
* Copyright (c) 2009-2017 Misakai Ltd.
* This program is free software: you can redistribute it and/or modify it under the
* terms of the GNU Affero General Public License as published by the  Free Software
* Foundation, either version 3 of the License, or(at your option) any later version.
*
* This program is distributed  in the hope that it  will be useful, but WITHOUT ANY
* WARRANTY;  without even  the implied warranty of MERCHANTABILITY or FITNESS FOR A
* PARTICULAR PURPOSE.  See the GNU Affero General Public License  for  more details.
*
* You should have  received a copy  of the  GNU Affero General Public License along
* with this program. If not, see<http://www.gnu.org/licenses/>.
************************************************************************************/

package config

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
)

// Constants used throughout the service.
const (
	ChannelSeparator = '/'   // The separator character.
	MaxMessageSize   = 65536 // Maximum message size allowed from/to the peer.
)

// SecretStore represents a contract for a store capable of resolving secrets.
type SecretStore interface {
	Configure(c *Config) error
	GetSecret(secretName string) (string, bool)
}

// NewDefault creates a default configuration.
func NewDefault() *Config {
	return &Config{
		ListenAddr: ":8080",
		Cluster: &ClusterConfig{
			ListenAddr:    ":4000",
			AdvertiseAddr: "public:4000",
			Passphrase:    "emitter-io",
		},
	}
}

// Config represents main configuration.
type Config struct {
	ListenAddr string         `json:"listen"`            // The API port used for TCP & Websocket communication.'
	License    string         `json:"license"`           // The port used for gossip.'
	TLS        *TLSConfig     `json:"tls,omitempty"`     // The API port used for Secure TCP & Websocket communication.'
	Vault      *VaultConfig   `json:"vault,omitempty"`   // The configuration for the Hashicorp Vault.
	Cluster    *ClusterConfig `json:"cluster,omitempty"` // The configuration for the clustering.
}

// TLSConfig represents TLS listener configuration.
type TLSConfig struct {
	ListenAddr  string `json:"listen"`      // The address to listen on.
	Certificate string `json:"certificate"` // The certificate request.
	PrivateKey  string `json:"private"`     // The private key for the certificate.
}

// Load loads a certificate from the configuration.
func (c *TLSConfig) Load() tls.Certificate {
	// If the certificate provided is in plain text, write to file so we can read it.
	if strings.HasPrefix(c.Certificate, "---") {
		if err := ioutil.WriteFile("broker.crt", []byte(c.Certificate), os.ModePerm); err == nil {
			c.Certificate = "broker.crt"
		}
	}

	// If the private key provided is in plain text, write to file so we can read it.
	if strings.HasPrefix(c.PrivateKey, "---") {
		if err := ioutil.WriteFile("broker.key", []byte(c.PrivateKey), os.ModePerm); err == nil {
			c.PrivateKey = "broker.key"
		}
	}

	// Make sure the paths are absolute, otherwise we won't be able to read the files.
	c.Certificate, _ = filepath.Abs(c.Certificate)
	c.PrivateKey, _ = filepath.Abs(c.PrivateKey)

	// Load the certificate from the cert/key files.
	cert, err := tls.LoadX509KeyPair(c.Certificate, c.PrivateKey)
	if err != nil {
		panic(err)
	}
	return cert
}

// VaultConfig represents Vault configuration.
type VaultConfig struct {
	Address     string `json:"address"` // The vault address to use.
	Application string `json:"app"`     // The vault application ID to use.
}

// ClusterConfig represents the configuration for the cluster.
type ClusterConfig struct {

	// The name of this node. This must be unique in the cluster. If this is not set, Emitter
	// will set it to the external IP address of the running machine.
	NodeName string `json:"name,omitempty"`

	// The IP address and port that is used to bind the inter-node communication network. This
	// is used for the actual binding of the port.
	ListenAddr string `json:"listen"`

	// The address and port to advertise inter-node communication network. This is used for nat
	// traversal.
	AdvertiseAddr string `json:"advertise"`

	// The seed address (or a domain name) for cluster join.
	Seed string `json:"seed"`

	// Passphrase is used to initialize the primary encryption key in a keyring. This key
	// is used for encrypting all the gossip messages (message-level encryption).
	Passphrase string `json:"passphrase,omitempty"`
}

// HasVault checks whether hashicorp vault endpoint is configured.
func (c *Config) HasVault() bool {
	return c.Vault != nil && c.Vault.Address != "" && c.Vault.Application != ""
}

// Write writes the configuration to a specific writer, in JSON format.
func (c *Config) write(output io.Writer) (int, error) {
	var formatted bytes.Buffer
	body, err := json.Marshal(c)
	if err != nil {
		return 0, err
	}

	if err := json.Indent(&formatted, body, "", "\t"); err != nil {
		return 0, err
	}

	return output.Write(formatted.Bytes())
}

// createDefault writes the default configuration to disk.
func createDefault(path string) (*Config, error) {
	f, err := os.OpenFile(path, os.O_CREATE, os.ModePerm)
	if err != nil {
		return nil, err
	}

	defer f.Close()
	c := NewDefault()
	if _, err := c.write(f); err != nil {
		return nil, err
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	return c, nil
}

// ReadOrCreate reads or creates the configuration object.
func ReadOrCreate(path string, stores ...SecretStore) (cfg *Config, err error) {
	cfg = new(Config)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		// Create a configuration and write it to a file
		if cfg, err = createDefault(path); err != nil {
			return nil, err
		}
	} else {
		// Read the config from file
		b, err := ioutil.ReadFile(path)
		if err != nil {
			return nil, err
		}

		// Unmarshal the configuration
		if err := json.Unmarshal(b, cfg); err != nil {
			return nil, err
		}
	}

	// Apply all the store overrides, in order
	for _, store := range stores {
		if err := store.Configure(cfg); err == nil {
			cfg.declassify("emitter", store)
		}
	}

	return cfg, nil
}

// Declassify traverses the configuration and resolves secrets.
func (c *Config) declassify(prefix string, provider SecretStore) {
	original := reflect.ValueOf(c)
	declassifyRecursive(prefix, provider, original)
}

// DeclassifyRecursive traverses the configuration and resolves secrets.
func declassifyRecursive(prefix string, provider SecretStore, value reflect.Value) {
	switch value.Kind() {
	case reflect.Ptr:
		if value.Elem().IsValid() {
			declassifyRecursive(prefix, provider, value.Elem())
		}

	// If it is a struct we translate each field
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			name := getFieldName(value.Type().Field(i))
			declassifyRecursive(prefix+"/"+name, provider, value.Field(i))
		}

	// This is a integer, we need to fetch the secret
	case reflect.Int:
		if v, ok := provider.GetSecret(prefix); ok {
			if iv, err := strconv.ParseInt(v, 10, 64); err == nil {
				value.SetInt(iv)
			}
		}

	// This is a string, we need to fetch the secret
	case reflect.String:
		if v, ok := provider.GetSecret(prefix); ok {
			value.SetString(v)
		}
	}
}

func getFieldName(f reflect.StructField) string {
	return strings.Replace(string(f.Tag.Get("json")), ",omitempty", "", -1)
}
