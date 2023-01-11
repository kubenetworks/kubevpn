package upgrade

import (
	"fmt"
	"testing"

	goversion "github.com/hashicorp/go-version"
	"github.com/stretchr/testify/assert"
	"golang.org/x/oauth2"
)

func TestCompare(t *testing.T) {
	version, err := goversion.NewVersion("v1.1.12")
	assert.Nil(t, err)

	newVersion, err := goversion.NewVersion("v1.1.13")
	assert.Nil(t, err)

	assert.True(t, version.LessThan(newVersion))
}

func TestGetManifest(t *testing.T) {
	client := oauth2.NewClient(nil, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "", TokenType: "Bearer"}))
	manifest, url, err := getManifest(client)
	assert.Nil(t, err)
	assert.True(t, manifest != "")
	assert.True(t, url != "")
}

func TestPercentage(t *testing.T) {
	per := float32(1) / float32(100) * 100
	s := fmt.Sprintf("%.0f%%", per)
	fmt.Printf(fmt.Sprintf("\r%s%%", s))
}
