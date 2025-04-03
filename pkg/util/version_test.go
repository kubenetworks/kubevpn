package util

import "testing"

func Test_newer(t *testing.T) {
	type args struct {
		clientVersionStr string
		clientImgStr     string
		serverImgStr     string
	}
	tests := []struct {
		name    string
		args    args
		want    bool
		wantErr bool
	}{
		// client version: v1.2.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		// server image: naison/kubevpn:v1.0.0
		{
			name: "Valid case - client(ghcr.io/kubenetworks/kubevpn:v1.2.1) newer than server(naison/kubevpn:v1.0.0)",
			args: args{
				clientVersionStr: "v1.2.1",
				clientImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "naison/kubevpn:v1.0.0",
			},
			want:    true,
			wantErr: false,
		},
		// client version: v1.2.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		// server image: docker.io/naison/kubevpn:v1.0.0
		{
			name: "Valid case - client(ghcr.io/kubenetworks/kubevpn:v1.2.1) newer than server(docker.io/naison/kubevpn:v1.0.0)",
			args: args{
				clientVersionStr: "v1.2.1",
				clientImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "docker.io/naison/kubevpn:v1.0.0",
			},
			want:    true,
			wantErr: false,
		},
		// client version: v1.2.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		// server image: naison/kubevpn:v1.2.1
		{
			name: "Valid case - client(ghcr.io/kubenetworks/kubevpn:v1.2.1) same as server(naison/kubevpn:v1.2.1)",
			args: args{
				clientVersionStr: "v1.2.1",
				clientImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "naison/kubevpn:v1.2.1",
			},
			want:    false,
			wantErr: false,
		},
		// client version: v1.2.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		// server image: docker.io/naison/kubevpn:v1.2.1
		{
			name: "Valid case - client(ghcr.io/kubenetworks/kubevpn:v1.2.1) same as server(docker.io/naison/kubevpn:v1.2.1)",
			args: args{
				clientVersionStr: "v1.2.1",
				clientImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "docker.io/naison/kubevpn:v1.2.1",
			},
			want:    false,
			wantErr: false,
		},
		// client version: v1.2.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		// server image: docker.io/naison/kubevpn:v1.3.1
		{
			name: "Valid case - client(ghcr.io/kubenetworks/kubevpn:v1.2.1) older as server(docker.io/naison/kubevpn:v1.3.1)",
			args: args{
				clientVersionStr: "v1.2.1",
				clientImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "docker.io/naison/kubevpn:v1.3.1",
			},
			want:    true,
			wantErr: true,
		},
		// client version: v1.3.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1 (not same as client version, --image=xxx)
		// server image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		{
			name: "Valid case - client cli version(v1.3.1) not same as client image(ghcr.io/kubenetworks/kubevpn:v1.2.1)",
			args: args{
				clientVersionStr: "v1.3.1",
				clientImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
			},
			want:    true,
			wantErr: true,
		},
		// client version: v1.2.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		// server image: ghcr.io/kubenetworks/kubevpn:v1.0.1
		{
			name: "Valid case - client(ghcr.io/kubenetworks/kubevpn:v1.2.1) newer than server(ghcr.io/kubenetworks/kubevpn:v1.0.1)",
			args: args{
				clientVersionStr: "v1.2.1",
				clientImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.0.1",
			},
			want:    true,
			wantErr: false,
		},
		// client version: v1.2.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		// server image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		{
			name: "Valid case - client(ghcr.io/kubenetworks/kubevpn:v1.2.1) same as server(ghcr.io/kubenetworks/kubevpn:v1.2.1)",
			args: args{
				clientVersionStr: "v1.2.1",
				clientImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
			},
			want:    false,
			wantErr: false,
		},
		// client version: v1.2.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		// server image: ghcr.io/kubenetworks/kubevpn:v1.3.1
		{
			name: "Valid case - client(ghcr.io/kubenetworks/kubevpn:v1.2.1) older as server(ghcr.io/kubenetworks/kubevpn:v1.3.1)",
			args: args{
				clientVersionStr: "v1.2.1",
				clientImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.3.1",
			},
			want:    true,
			wantErr: true,
		},

		// custom server image registry, but client image is not same as client version, does not upgrade
		// client version: v1.2.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		// server image: mykubevpn.io/kubenetworks/kubevpn:v1.1.1
		{
			name: "custom server image registry, but client image is not same as client version, does not upgrade",
			args: args{
				clientVersionStr: "v1.2.1",
				clientImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "mykubevpn.io/kubenetworks/kubevpn:v1.1.1",
			},
			want:    true,
			wantErr: false,
		},

		// custom server image registry, client image is same as client version,  upgrade
		// client version: v1.2.1
		// client image: ghcr.io/kubenetworks/kubevpn:v1.2.1
		// server image: mykubevpn.io/kubenetworks/kubevpn:v1.1.1
		{
			name: "custom server image registry, client image is same as client version,  upgrade",
			args: args{
				clientVersionStr: "v1.2.1",
				clientImgStr:     "mykubevpn.io/kubenetworks/kubevpn:v1.2.1",
				serverImgStr:     "mykubevpn.io/kubenetworks/kubevpn:v1.1.1",
			},
			want:    true,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := IsNewer(tt.args.clientVersionStr, tt.args.clientImgStr, tt.args.serverImgStr)
			if (err != nil) != tt.wantErr {
				t.Errorf("newer() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("newer() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetTargetImage(t *testing.T) {
	type args struct {
		version string
		image   string
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		{
			name: "replace version",
			args: args{
				version: "v1.2.3",
				image:   "ghcr.io/kubenetworks/kubevpn:v1.0.0",
			},
			want: "ghcr.io/kubenetworks/kubevpn:v1.2.3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GetTargetImage(tt.args.version, tt.args.image); got != tt.want {
				t.Errorf("GetTargetImage() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsVersionMajorOrMinorDiff(t *testing.T) {
	type args struct {
		clientVersionStr string
		serverImgStr     string
	}
	tests := []struct {
		name string
		args args
		want bool
	}{
		{
			name: "Major version is diff",
			args: args{
				clientVersionStr: "v2.2.3",
				serverImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.2.0",
			},
			want: true,
		},
		{
			name: "Minor version is diff",
			args: args{
				clientVersionStr: "v1.2.3",
				serverImgStr:     "ghcr.io/kubenetworks/kubevpn:v1.0.0",
			},
			want: true,
		},
		{
			name: "PATCH version is diff",
			args: args{
				clientVersionStr: "v2.2.3",
				serverImgStr:     "ghcr.io/kubenetworks/kubevpn:v2.2.0",
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CmpClientVersionAndPodImageTag(tt.args.clientVersionStr, tt.args.serverImgStr); (got != 0) != tt.want {
				t.Errorf("CmpClientVersionAndPodImageTag() = %v, want %v", got, tt.want)
			}
		})
	}
}
