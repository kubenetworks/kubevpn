package util

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"strings"
	"time"
	"unsafe"

	"github.com/jcmturner/gofork/encoding/asn1"
	"github.com/jcmturner/gokrb5/v8/types"
)

const (
	headerFieldTagKDCOffset = 1
)

// CCache is the file credentials cache as define here: https://web.mit.edu/kerberos/krb5-latest/doc/formats/ccache_file_format.html
type CCache struct {
	Version          uint8
	Header           header
	DefaultPrincipal principal
	Credentials      []*Credential
	Path             string
}

type header struct {
	length uint16
	fields []headerField
}

type headerField struct {
	tag    uint16
	length uint16
	value  []byte
}

// Credential cache entry principal struct.
type principal struct {
	Realm         string
	PrincipalName types.PrincipalName
}

// Credential holds a Kerberos client's ccache credential information.
type Credential struct {
	Client       principal
	Server       principal
	Key          types.EncryptionKey
	AuthTime     time.Time
	StartTime    time.Time
	EndTime      time.Time
	RenewTill    time.Time
	IsSKey       bool
	TicketFlags  asn1.BitString
	Addresses    []types.HostAddress
	AuthData     []types.AuthorizationDataEntry
	Ticket       []byte
	SecondTicket []byte
}

// LoadCCache loads a credential cache file into a CCache type.
func LoadCCache(cpath string) (*CCache, error) {
	c := new(CCache)
	b, err := os.ReadFile(cpath)
	if err != nil {
		return c, err
	}
	err = c.Unmarshal(b)
	return c, err
}

// NewV4CCache creates a new Version 4 CCache with no credentials in it
func NewV4CCache() *CCache {
	c := CCache{}
	c.Version = 4
	h := header{}
	h.length = 0
	c.Header = h
	return &c
}

// AddCredential adds a credential to a CCache
func (c *CCache) AddCredential(cred *Credential) {
	c.Credentials = append(c.Credentials, cred)
}

// SetDefaultPrincipal sets the default principal for a CCache
func (c *CCache) SetDefaultPrincipal(d principal) {
	c.DefaultPrincipal = d
}

// NewPrincipal returns a new principal
func NewPrincipal(name types.PrincipalName, realm string) principal {
	p := principal{}
	p.Realm = realm
	p.PrincipalName = name
	return p
}

// Unmarshal a byte slice of credential cache data into CCache type.
func (c *CCache) Unmarshal(b []byte) error {
	p := 0
	//The first byte of the file always has the value 5
	if int8(b[p]) != 5 {
		return errors.New("Invalid credential cache data. First byte does not equal 5")
	}
	p++
	//Get credential cache version
	//The second byte contains the version number (1 to 4)
	c.Version = b[p]
	if c.Version < 1 || c.Version > 4 {
		return errors.New("Invalid credential cache data. Keytab version is not within 1 to 4")
	}
	p++
	endian := c.getEndian()
	if c.Version == 4 {
		err := parseHeader(b, &p, c, endian)
		if err != nil {
			return err
		}
	}
	c.DefaultPrincipal = parsePrincipal(b, &p, c, endian)
	for p < len(b) {
		cred, err := parseCredential(b, &p, c, endian)
		if err != nil {
			return err
		}
		c.Credentials = append(c.Credentials, cred)
	}
	return nil
}

// Marshal a CCache type into a byte string
func (c *CCache) Marshal() ([]byte, error) {
	var b bytes.Buffer
	var err error
	endian := c.getEndian()

	// The first byte of the file always has the value 5
	err = b.WriteByte(5)
	if err != nil {
		return []byte{}, err
	}

	// Write the CCache version
	err = b.WriteByte(c.Version)

	if c.Version == 4 {
		// Write version 4 header
		headerBytes, err := c.writeV4Header()
		if err != nil {
			return []byte{}, err
		}

		_, err = b.Write(headerBytes)
		if err != nil {
			return []byte{}, err
		}
	}

	// Write default principal
	princBytes, err := c.writePrincipal(c.DefaultPrincipal, endian)
	if err != nil {
		return []byte{}, err
	}

	_, err = b.Write(princBytes)
	if err != nil {
		return []byte{}, err
	}

	// Write credentials
	for _, cred := range c.Credentials {
		credBytes, err := c.writeCredential(cred, endian)
		if err != nil {
			return []byte{}, err
		}
		_, err = b.Write(credBytes)
		if err != nil {
			return []byte{}, err
		}
	}

	return b.Bytes(), nil
}

func (c *CCache) writeV4Header() ([]byte, error) {
	var byteString bytes.Buffer
	var err error

	b := &byteString

	// V4 is always BigEndian
	endian := binary.BigEndian

	// Write header length
	err = binary.Write(b, endian, c.Header.length)
	if err != nil {
		return []byte{}, err
	}

	for _, field := range c.Header.fields {
		// Write field tag
		err = binary.Write(b, endian, field.tag)
		if err != nil {
			return []byte{}, err
		}

		// Write field length
		err = binary.Write(b, endian, field.length)
		if err != nil {
			return []byte{}, err
		}

		// Write field data
		_, err = b.Write(field.value)
		if err != nil {
			return []byte{}, err
		}
	}

	return byteString.Bytes(), nil
}

func (c *CCache) writePrincipal(p principal, endian *binary.ByteOrder) ([]byte, error) {
	var byteString bytes.Buffer
	var err error

	b := &byteString

	// Version 1 does not have the name type
	if c.Version != 1 {
		err = binary.Write(b, *endian, uint32(p.PrincipalName.NameType))
		if err != nil {
			return []byte{}, err
		}
	}

	// Count of components
	componentCount := len(p.PrincipalName.NameString)
	if c.Version == 1 {
		// Version 1 includes realm in count
		componentCount = componentCount + 1
	}
	err = binary.Write(b, *endian, uint32(componentCount))
	if err != nil {
		return []byte{}, err
	}

	// Realm --- Length then data

	realmLength := len(p.Realm)
	err = binary.Write(b, *endian, uint32(realmLength))
	if err != nil {
		return []byte{}, err
	}

	_, err = b.WriteString(p.Realm)
	if err != nil {
		return []byte{}, err
	}

	// Components
	for _, namePart := range p.PrincipalName.NameString {
		// length then data
		err = binary.Write(b, *endian, uint32(len(namePart)))
		if err != nil {
			return []byte{}, err
		}

		_, err = b.WriteString(namePart)
		if err != nil {
			return []byte{}, err
		}
	}

	return byteString.Bytes(), nil

}

func (c *CCache) writeCredential(cred *Credential, endian *binary.ByteOrder) ([]byte, error) {
	var byteString bytes.Buffer
	var err error

	b := &byteString

	// Client - a principal
	princBytes, err := c.writePrincipal(cred.Client, endian)
	if err != nil {
		return []byte{}, err
	}
	_, err = b.Write(princBytes)
	if err != nil {
		return []byte{}, err
	}

	// Server - a principal
	princBytes, err = c.writePrincipal(cred.Server, endian)
	if err != nil {
		return []byte{}, err
	}
	_, err = b.Write(princBytes)
	if err != nil {
		return []byte{}, err
	}

	// Key - 16 bit key type, then key data
	err = binary.Write(b, *endian, uint16(cred.Key.KeyType))
	if err != nil {
		return []byte{}, err
	}

	if c.Version == 3 {
		// Version 3 repeats the key type for some reason
		err = binary.Write(b, *endian, uint32(cred.Key.KeyType))
		if err != nil {
			return []byte{}, err
		}
	}

	keyLen := len(cred.Key.KeyValue)
	err = binary.Write(b, *endian, uint32(keyLen))
	if err != nil {
		return []byte{}, err
	}

	_, err = b.Write(cred.Key.KeyValue)
	if err != nil {
		return []byte{}, err
	}

	// AuthTime, StartTime, EndTime, RewnewTil - all 32 bit
	// Unix Epoch seconds
	for _, timeValue := range []time.Time{cred.AuthTime, cred.StartTime, cred.EndTime, cred.RenewTill} {
		err = binary.Write(b, *endian, uint32(timeValue.Unix()))
		if err != nil {
			return []byte{}, err
		}
	}

	// IsSKey
	isSKey := uint8(0)
	if cred.IsSKey {
		isSKey = uint8(1)
	}
	err = binary.Write(b, *endian, isSKey)
	if err != nil {
		return []byte{}, err
	}

	// TicketFlags
	err = binary.Write(b, *endian, cred.TicketFlags.Bytes)
	if err != nil {
		return []byte{}, err
	}

	// Addresses
	// Address count first
	err = binary.Write(b, *endian, uint32(len(cred.Addresses)))
	if err != nil {
		return []byte{}, err
	}

	// Then each address
	for _, address := range cred.Addresses {
		// Type
		err = binary.Write(b, *endian, uint16(address.AddrType))
		if err != nil {
			return []byte{}, err
		}
		// Data length
		err = binary.Write(b, *endian, uint32(len(address.Address)))
		if err != nil {
			return []byte{}, err
		}
		// Data
		_, err = b.Write(address.Address)
		if err != nil {
			return []byte{}, err
		}
	}

	// AuthData
	// AuthData count first
	err = binary.Write(b, *endian, uint32(len(cred.AuthData)))
	if err != nil {
		return []byte{}, err
	}

	// Then each auth data
	for _, authData := range cred.AuthData {
		// Type
		err = binary.Write(b, *endian, uint16(authData.ADType))
		if err != nil {
			return []byte{}, err
		}
		// Data length
		err = binary.Write(b, *endian, uint32(len(authData.ADData)))
		if err != nil {
			return []byte{}, err
		}
		// Data
		_, err = b.Write(authData.ADData)
		if err != nil {
			return []byte{}, err
		}
	}

	// Ticket
	err = binary.Write(b, *endian, uint32(len(cred.Ticket)))
	if err != nil {
		return []byte{}, err
	}
	_, err = b.Write(cred.Ticket)
	if err != nil {
		return []byte{}, err
	}

	// Second Ticket
	err = binary.Write(b, *endian, uint32(len(cred.SecondTicket)))
	if err != nil {
		return []byte{}, err
	}
	_, err = b.Write(cred.SecondTicket)
	if err != nil {
		return []byte{}, err
	}

	return byteString.Bytes(), nil
}

// Return either binary.ByteOrder depending on the CCache
// version and machine endianess
func (c *CCache) getEndian() *binary.ByteOrder {
	var endian binary.ByteOrder
	endian = binary.BigEndian
	//Version 1 or 2 of the file format uses native byte order for integer representations. Versions 3 & 4 always uses big-endian byte order
	if (c.Version == 1 || c.Version == 2) && isNativeEndianLittle() {
		endian = binary.LittleEndian
	}

	return &endian
}

func parseHeader(b []byte, p *int, c *CCache, e *binary.ByteOrder) error {
	if c.Version != 4 {
		return errors.New("Credentials cache version is not 4 so there is no header to parse.")
	}
	h := header{}
	h.length = uint16(readInt16(b, p, e))
	for *p <= int(h.length) {
		f := headerField{}
		f.tag = uint16(readInt16(b, p, e))
		f.length = uint16(readInt16(b, p, e))
		f.value = b[*p : *p+int(f.length)]
		*p += int(f.length)
		if !f.valid() {
			return errors.New("Invalid credential cache header found")
		}
		h.fields = append(h.fields, f)
	}
	c.Header = h
	return nil
}

// Parse the Keytab bytes of a principal into a Keytab entry's principal.
func parsePrincipal(b []byte, p *int, c *CCache, e *binary.ByteOrder) (princ principal) {
	if c.Version != 1 {
		//Name Type is omitted in version 1
		princ.PrincipalName.NameType = readInt32(b, p, e)
	}
	nc := int(readInt32(b, p, e))
	if c.Version == 1 {
		//In version 1 the number of components includes the realm. Minus 1 to make consistent with version 2
		nc--
	}
	lenRealm := readInt32(b, p, e)
	princ.Realm = string(readBytes(b, p, int(lenRealm), e))
	for i := 0; i < nc; i++ {
		l := readInt32(b, p, e)
		princ.PrincipalName.NameString = append(princ.PrincipalName.NameString, string(readBytes(b, p, int(l), e)))
	}
	return princ
}

func parseCredential(b []byte, p *int, c *CCache, e *binary.ByteOrder) (cred *Credential, err error) {
	cred = new(Credential)
	cred.Client = parsePrincipal(b, p, c, e)
	cred.Server = parsePrincipal(b, p, c, e)
	key := types.EncryptionKey{}
	key.KeyType = int32(readInt16(b, p, e))
	if c.Version == 3 {
		//repeated twice in version 3
		key.KeyType = int32(readInt16(b, p, e))
	}
	key.KeyValue = readData(b, p, e)
	cred.Key = key
	cred.AuthTime = readTimestamp(b, p, e)
	cred.StartTime = readTimestamp(b, p, e)
	cred.EndTime = readTimestamp(b, p, e)
	cred.RenewTill = readTimestamp(b, p, e)
	if ik := readInt8(b, p, e); ik == 0 {
		cred.IsSKey = false
	} else {
		cred.IsSKey = true
	}
	cred.TicketFlags = types.NewKrbFlags()
	cred.TicketFlags.Bytes = readBytes(b, p, 4, e)
	l := int(readInt32(b, p, e))
	cred.Addresses = make([]types.HostAddress, l, l)
	for i := range cred.Addresses {
		cred.Addresses[i] = readAddress(b, p, e)
	}
	l = int(readInt32(b, p, e))
	cred.AuthData = make([]types.AuthorizationDataEntry, l, l)
	for i := range cred.AuthData {
		cred.AuthData[i] = readAuthDataEntry(b, p, e)
	}
	cred.Ticket = readData(b, p, e)
	cred.SecondTicket = readData(b, p, e)
	return
}

// GetClientPrincipalName returns a PrincipalName type for the client the credentials cache is for.
func (c *CCache) GetClientPrincipalName() types.PrincipalName {
	return c.DefaultPrincipal.PrincipalName
}

// GetClientRealm returns the reals of the client the credentials cache is for.
func (c *CCache) GetClientRealm() string {
	return c.DefaultPrincipal.Realm
}

//// GetClientCredentials returns a Credentials object representing the client of the credentials cache.
//func (c *CCache) GetClientCredentials() *Credentials {
//	return &Credentials{
//		username: c.DefaultPrincipal.PrincipalName.PrincipalNameString(),
//		realm:    c.GetClientRealm(),
//		cname:    c.DefaultPrincipal.PrincipalName,
//	}
//}

// Contains tests if the cache contains a credential for the provided server PrincipalName
func (c *CCache) Contains(p types.PrincipalName) bool {
	for _, cred := range c.Credentials {
		if cred.Server.PrincipalName.Equal(p) {
			return true
		}
	}
	return false
}

// GetEntry returns a specific credential for the PrincipalName provided.
func (c *CCache) GetEntry(p types.PrincipalName) (*Credential, bool) {
	cred := new(Credential)
	var found bool
	for i := range c.Credentials {
		if c.Credentials[i].Server.PrincipalName.Equal(p) {
			cred = c.Credentials[i]
			found = true
			break
		}
	}
	if !found {
		return cred, false
	}
	return cred, true
}

// GetEntries filters out configuration entries an returns a slice of credentials.
func (c *CCache) GetEntries() []*Credential {
	creds := make([]*Credential, 0)
	for _, cred := range c.Credentials {
		// Filter out configuration entries
		if strings.HasPrefix(cred.Server.Realm, "X-CACHECONF") {
			continue
		}
		creds = append(creds, cred)
	}
	return creds
}

func (h *headerField) valid() bool {
	// See https://web.mit.edu/kerberos/krb5-latest/doc/formats/ccache_file_format.html - Header format
	switch h.tag {
	case headerFieldTagKDCOffset:
		if h.length != 8 || len(h.value) != 8 {
			return false
		}
		return true
	}
	return false
}

func readData(b []byte, p *int, e *binary.ByteOrder) []byte {
	l := readInt32(b, p, e)
	return readBytes(b, p, int(l), e)
}

func readAddress(b []byte, p *int, e *binary.ByteOrder) types.HostAddress {
	a := types.HostAddress{}
	a.AddrType = int32(readInt16(b, p, e))
	a.Address = readData(b, p, e)
	return a
}

func readAuthDataEntry(b []byte, p *int, e *binary.ByteOrder) types.AuthorizationDataEntry {
	a := types.AuthorizationDataEntry{}
	a.ADType = int32(readInt16(b, p, e))
	a.ADData = readData(b, p, e)
	return a
}

// Read bytes representing a timestamp.
func readTimestamp(b []byte, p *int, e *binary.ByteOrder) time.Time {
	return time.Unix(int64(readInt32(b, p, e)), 0)
}

// Read bytes representing an eight bit integer.
func readInt8(b []byte, p *int, e *binary.ByteOrder) (i int8) {
	buf := bytes.NewBuffer(b[*p : *p+1])
	binary.Read(buf, *e, &i)
	*p++
	return
}

// Read bytes representing a sixteen bit integer.
func readInt16(b []byte, p *int, e *binary.ByteOrder) (i int16) {
	buf := bytes.NewBuffer(b[*p : *p+2])
	binary.Read(buf, *e, &i)
	*p += 2
	return
}

// Read bytes representing a thirty two bit integer.
func readInt32(b []byte, p *int, e *binary.ByteOrder) (i int32) {
	buf := bytes.NewBuffer(b[*p : *p+4])
	binary.Read(buf, *e, &i)
	*p += 4
	return
}

func readBytes(b []byte, p *int, s int, e *binary.ByteOrder) []byte {
	buf := bytes.NewBuffer(b[*p : *p+s])
	r := make([]byte, s)
	binary.Read(buf, *e, &r)
	*p += s
	return r
}

func isNativeEndianLittle() bool {
	var x = 0x012345678
	var p = unsafe.Pointer(&x)
	var bp = (*[4]byte)(p)

	var endian bool
	if 0x01 == bp[0] {
		endian = false
	} else if (0x78 & 0xff) == (bp[0] & 0xff) {
		endian = true
	} else {
		// Default to big endian
		endian = false
	}
	return endian
}
