package dns

import (
	"bytes"
	"text/template"

	"github.com/wencaiwulue/kubevpn/pkg/errors"
)

type CoreFile struct {
	Content []byte
}

// Body Gets the Caddyfile contents
func (c *CoreFile) Body() []byte {
	return c.Content
}

// Path Gets the path to the origin file
func (c *CoreFile) Path() string {
	return "CoreFile"
}

// ServerType The type of server this input is intended for
func (c *CoreFile) ServerType() string {
	return "dns"
}

type CoreFileTmpl struct {
	UpstreamDNS string
	Nameservers string
}

func BuildCoreFile(corefileTmpl CoreFileTmpl) (*CoreFile, error) {
	tplText := `
.:53 {
    bind 127.0.0.1
    forward cluster.local {{ .UpstreamDNS }}
    forward . {{ .Nameservers }} {{ .UpstreamDNS }} {
	policy sequential
	max_concurrent 1
    }
    cache 30
    log
    errors
    reload
}`

	tpl, err := template.New("corefile").Parse(tplText)
	if err != nil {
		err = errors.Wrap(err, "Failed to parse template for corefile")
		return nil, err
	}

	data := bytes.NewBuffer(nil)
	if err := tpl.Execute(data, corefileTmpl); err != nil {
		return nil, err
	}

	return &CoreFile{
		Content: data.Bytes(),
	}, nil
}
