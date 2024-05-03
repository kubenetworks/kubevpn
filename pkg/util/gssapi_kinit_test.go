package util

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/jcmturner/gofork/encoding/asn1"
	"github.com/jcmturner/gokrb5/v8/client"
	"github.com/jcmturner/gokrb5/v8/iana/nametype"
	"github.com/jcmturner/gokrb5/v8/types"
)

// depends on this pr: https://github.com/jcmturner/gokrb5/pull/423
func testKinit(t *testing.T) {
	c, err := NewKrb5InitiatorClientWithPassword(
		"fengcaiwen",
		"xxx",
		GetKrb5Path(),
	)
	if err != nil {
		panic(err)
	}
	_, key, err := c.client.GetServiceTicket("krbtgt/BYTEDANCE.COM")
	_, key1, err := c.client.GetServiceTicket("host/10.37.6.14")
	if err != nil {
		panic(err)
	}
	newPrincipal := NewPrincipal(c.client.Credentials.CName(), c.client.Credentials.Realm())
	cCache := CCache{
		Version:          4,
		DefaultPrincipal: newPrincipal,
		Path:             "",
	}
	value := reflect.ValueOf(c.client).Elem().FieldByName("cache")
	value = reflect.NewAt(value.Type(), unsafe.Pointer(value.UnsafeAddr())).Elem()
	clientCache := value.Interface().(*client.Cache)
	for _, entry := range clientCache.Entries {
		marshal, _ := entry.Ticket.Marshal()
		var flags asn1.BitString
		if entry.Ticket.DecryptedEncPart.Flags.BitLength != 0 {
			flags = entry.Ticket.DecryptedEncPart.Flags
		} else {
			flags = asn1.BitString{
				Bytes:     []byte{80, 97, 0, 0},
				BitLength: 32,
			}
		}
		var keyV types.EncryptionKey
		var ntype int32
		if strings.Contains(entry.SPN, "krbtgt/BYTEDANCE.COM") {
			keyV = key
			ntype = nametype.KRB_NT_SRV_INST
		} else {
			keyV = key1
			ntype = nametype.KRB_NT_SRV_HST
		}
		var renew = entry.RenewTill
		if renew.IsZero() {
			renew = time.Unix(0, 0)
		}
		cCache.AddCredential(&Credential{
			Client:       newPrincipal,
			Server:       NewPrincipal(types.NewPrincipalName(ntype, entry.SPN), c.client.Credentials.Realm()),
			Key:          keyV,
			AuthTime:     entry.AuthTime.In(time.Local),
			StartTime:    entry.StartTime.In(time.Local),
			EndTime:      entry.EndTime.In(time.Local),
			RenewTill:    renew,
			IsSKey:       false,
			TicketFlags:  flags,
			Addresses:    entry.Ticket.DecryptedEncPart.CAddr,
			AuthData:     entry.Ticket.DecryptedEncPart.AuthorizationData,
			Ticket:       marshal,
			SecondTicket: nil,
		})
	}
	marshal, err := cCache.Marshal()
	if err != nil {
		panic(err)
	}
	err = os.WriteFile("/tmp/krb5cc_0", marshal, 0600)
	if err != nil {
		panic(err)
	}
	t.Log("CCache file created at /tmp/krb5cc_0")
	os.Setenv("KRB5CCNAME", "/tmp/krb5cc_0")
	t.Log(os.Getenv("KRB5CCNAME"))
}
