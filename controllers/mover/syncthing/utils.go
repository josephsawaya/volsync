package syncthing

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"io/ioutil"
	"math/big"
	"net"
	"net/http"
	"time"

	"github.com/backube/volsync/api/v1alpha1"
)

// UpdateDevices Updates the Syncthing's connected devices with the provided peerList.
func (st *Syncthing) UpdateDevices(peerList []v1alpha1.SyncthingPeer) {
	st.logger.V(4).Info("Updating devices", "peerlist", peerList)

	// update syncthing config based on the provided peerlist
	newDevices := []SyncthingDevice{}
	// add myself to the device list
	for _, device := range st.Config.Devices {
		if device.DeviceID == st.SystemStatus.MyID {
			newDevices = append(newDevices, device)
			break
		}
	}

	for _, device := range peerList {
		// skip self
		if device.ID == st.SystemStatus.MyID {
			continue
		}
		stDeviceToAdd := SyncthingDevice{
			DeviceID:   device.ID,
			Addresses:  []string{device.Address},
			Name:       "Syncthing Device " + string(rune(len(newDevices))),
			Introducer: device.Introducer,
		}
		st.logger.V(4).Info("Adding device: %+v\n", stDeviceToAdd)
		newDevices = append(newDevices, stDeviceToAdd)
	}

	st.Config.Devices = newDevices
	st.logger.V(4).Info("Updated devices", "devices", st.Config.Devices)

	// update the folders
	st.UpdateFolders()
}

// UpdateFolders Updates the Syncthing folders to be shared with its set of devices.
func (st *Syncthing) UpdateFolders() {
	// share the current folder(s) with the new devices
	var newFolders = []SyncthingFolder{}
	for _, folder := range st.Config.Folders {
		// copy folder & reset
		newFolder := folder
		newFolder.Devices = []FolderDeviceConfiguration{}
		for _, device := range st.Config.Devices {
			newFolder.Devices = append(newFolder.Devices, FolderDeviceConfiguration{
				DeviceID: device.DeviceID,
			})
		}
		newFolders = append(newFolders, newFolder)
	}
	st.Config.Folders = newFolders
}

// NeedsReconfigure Determines whether the given nodeList differs from Syncthing's internal devices.
func (st *Syncthing) NeedsReconfigure(nodeList []v1alpha1.SyncthingPeer) bool {
	// check if the syncthing nodelist diverges from the current syncthing devices
	var newDevices map[string]v1alpha1.SyncthingPeer = map[string]v1alpha1.SyncthingPeer{
		st.SystemStatus.MyID: {
			ID:      st.SystemStatus.MyID,
			Address: "",
		},
	}
	for _, device := range nodeList {
		newDevices[device.ID] = device
	}

	// create a map for current devices
	var currentDevs map[string]v1alpha1.SyncthingPeer = map[string]v1alpha1.SyncthingPeer{}
	for _, device := range st.Config.Devices {
		currentDevs[device.DeviceID] = v1alpha1.SyncthingPeer{
			ID:      device.DeviceID,
			Address: device.Addresses[0],
		}
	}

	// check if the syncthing nodelist diverges from the current syncthing devices
	for _, device := range newDevices {
		if _, ok := currentDevs[device.ID]; !ok {
			return true
		}
	}
	for _, device := range currentDevs {
		if _, ok := newDevices[device.ID]; !ok {
			return true
		}
	}
	return false
}

// FetchLatestInfo Updates the Syncthing object with the latest data fetched from the Syncthing API.
func (st *Syncthing) FetchLatestInfo() error {
	if err := st.FetchSyncthingConfig(); err != nil {
		return err
	}
	if err := st.FetchSyncthingSystemStatus(); err != nil {
		return err
	}
	if err := st.FetchConnectedStatus(); err != nil {
		return err
	}
	return nil
}

// UpdateSyncthingConfig Updates the Syncthing config with the locally-stored config.
func (st *Syncthing) UpdateSyncthingConfig() error {
	// update the config
	st.logger.V(4).Info("Updating Syncthing config")
	_, err := st.jsonRequest("/rest/config", "PUT", st.Config)
	if err != nil {
		st.logger.V(4).Error(err, "Failed to update Syncthing config")
		return err
	}
	return err
}

// FetchSyncthingConfig fetches the Syncthing config and updates the config.
func (st *Syncthing) FetchSyncthingConfig() error {
	responseBody := &SyncthingConfig{
		Devices: []SyncthingDevice{},
		Folders: []SyncthingFolder{},
	}
	st.logger.V(4).Info("Fetching Syncthing config")
	data, err := st.jsonRequest("/rest/config", "GET", nil)
	if err != nil {
		return err
	}
	err = json.Unmarshal(data, responseBody)
	st.Config = responseBody
	return err
}

// FetchSyncthingSystemStatus fetches the Syncthing system status.
func (st *Syncthing) FetchSyncthingSystemStatus() error {
	responseBody := &SystemStatus{}
	st.logger.V(4).Info("Fetching Syncthing system status")
	data, err := st.jsonRequest("/rest/system/status", "GET", nil)
	if err != nil {
		return err
	}
	// unmarshal the data into the responseBody
	err = json.Unmarshal(data, responseBody)
	st.SystemStatus = responseBody
	return err
}

// FetchConnectedStatus Fetches the connection status of the syncthing instance.
func (st *Syncthing) FetchConnectedStatus() error {
	// updates the connected status if successful, else returns an error
	responseBody := &SystemConnections{
		Connections: map[string]ConnectionStats{},
	}
	st.logger.V(4).Info("Fetching Syncthing connected status")
	data, err := st.jsonRequest("/rest/system/connections", "GET", nil)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(data, responseBody); err == nil {
		st.SystemConnections = responseBody
	}
	return err
}

// jsonRequest performs a request to the Syncthing API and returns the response body.
//nolint:funlen,lll,unparam,unused
func (st *Syncthing) jsonRequest(endpoint string, method string, requestBody interface{}) ([]byte, error) {
	// marshal above json body into a string
	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		return nil, err
	}
	// tostring the json body
	body := io.Reader(bytes.NewReader(jsonBody))

	// build new client if none exists
	req, err := http.NewRequest(method, st.APIConfig.APIURL+endpoint, body)

	// set headers
	for key, value := range st.APIConfig.Headers() {
		req.Header.Set(key, value)
	}

	// make an HTTPS POST request
	if err != nil {
		return nil, err
	}
	resp, err := st.APIConfig.Client.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != 200 {
		return nil, errors.New("HTTP status code is not 200")
	}

	// read body into response
	return ioutil.ReadAll(resp.Body)
}

// Headers Returns a map containing the necessary headers for Syncthing API requests.
func (api *APIConfig) Headers() map[string]string {
	return map[string]string{
		"X-API-Key":    api.APIKey,
		"Content-Type": "application/json",
	}
}

// BuildTLSClient Returns a new TLS client for Syncthing API requests.
func (api *APIConfig) BuildOrUseExistingTLSClient() *http.Client {
	if api.Client != nil {
		return api.Client
	}
	return api.BuildTLSClient()
}

// BuildTLSClient Returns a new TLS client for Syncthing API requests.
func (api *APIConfig) BuildTLSClient() *http.Client {
	tlsConfig := api.TLSConfig
	if tlsConfig == nil {
		tlsConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
	}

	// load the TLS config with certificates
	tr := &http.Transport{
		TLSClientConfig: tlsConfig,
	}
	client := &http.Client{
		Transport: tr,
		Timeout:   time.Second * 5,
	}
	return client
}

// GenerateRandomBytes Generates random bytes of the given length using the OS's RNG.
func GenerateRandomBytes(length int) ([]byte, error) {
	// generates random bytes of given length
	b := make([]byte, length)
	_, err := rand.Read(b)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// GenerateRandomString Generates a random string of the given length using the OS's RNG.
func GenerateRandomString(length int) (string, error) {
	// generate a random string
	b, err := GenerateRandomBytes(length)
	return base64.URLEncoding.EncodeToString(b), err
}

// GetRedHatTLSName returns information for the TLS certificate we will generate for Syncthing.
func GetRedHatTLSName() pkix.Name {
	return pkix.Name{
		Organization:  []string{"Red Hat, INC."},
		Country:       []string{"US"},
		Province:      []string{"Massachusetts"},
		Locality:      []string{"Boston"},
		StreetAddress: []string{"300 A Street"},
		PostalCode:    []string{"02210"},
	}
}

// GenerateTLSCertificatesForSyncthing generates a self-signed PEM-encoded certificate and key for Syncthing
// which the VolSync client and Syncthing API Server will use to communicate with each other.
//nolint:funlen
func GenerateTLSCertificatesForSyncthing(
	APIServiceAddress string,
) (*bytes.Buffer, *bytes.Buffer, error) {
	// TODO: Change from RSA to ECDSA
	// TODO: split this up into several files
	// we will need to perform checks if the apiServiceDNS has changed
	// and re-generate in case the TLS Certificates have changed

	// serial number should be the current time in unix epoch time
	TLSName := GetRedHatTLSName()

	// setup expiry dates
	notBefore := time.Now()
	// expire in 10 years
	notAfter := notBefore.AddDate(10, 0, 0)

	// generate a bunch of random bytes
	serialNumber, err := GenerateRandomBytes(2048)
	if err != nil {
		return nil, nil, err
	}
	// convert the serial number to a bigint
	serialNumberBigInt := new(big.Int).SetBytes(serialNumber)

	// set up our CA certificate
	ca := &x509.Certificate{
		SerialNumber:          serialNumberBigInt,
		Subject:               TLSName,
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}

	// create our private and public key
	caPrivKey, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return nil, nil, err
	}

	// create the CA
	caBytes, err := x509.CreateCertificate(rand.Reader, ca, ca, &caPrivKey.PublicKey, caPrivKey)
	if err != nil {
		return nil, nil, err
	}

	// pem encode
	caPEM := new(bytes.Buffer)
	err = pem.Encode(caPEM, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: caBytes,
	})
	if err != nil {
		return nil, nil, err
	}

	caPrivKeyPEM := new(bytes.Buffer)
	err = pem.Encode(caPrivKeyPEM, &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(caPrivKey),
	})
	if err != nil {
		return nil, nil, err
	}

	// generate another random serial number
	serialNumber, err = GenerateRandomBytes(2048)
	if err != nil {
		return nil, nil, err
	}
	// convert the serial number to a bigint
	serialNumberBigInt = new(big.Int).SetBytes(serialNumber)

	// set up our server certificate
	cert := &x509.Certificate{
		SerialNumber: serialNumberBigInt,
		Subject:      TLSName,
		DNSNames:     []string{APIServiceAddress},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}

	certPrivKey, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return nil, nil, err
	}

	certBytes, err := x509.CreateCertificate(rand.Reader, cert, ca, &certPrivKey.PublicKey, caPrivKey)
	if err != nil {
		return nil, nil, err
	}

	certPEM := new(bytes.Buffer)
	err = pem.Encode(certPEM, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certBytes,
	})
	if err != nil {
		return nil, nil, err
	}

	certPrivKeyPEM := new(bytes.Buffer)
	err = pem.Encode(certPrivKeyPEM, &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(certPrivKey),
	})
	if err != nil {
		return nil, nil, err
	}

	return certPEM, certPrivKeyPEM, nil
}
