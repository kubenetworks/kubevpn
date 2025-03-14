package util

import (
	"fmt"

	"github.com/distribution/reference"
	"github.com/hashicorp/go-version"
	"github.com/pkg/errors"
)

// CmpClientVersionAndClientImage
/**
version: MAJOR.MINOR.PATCH

client version should match client image
MAJOR and MINOR different should be same, otherwise just exit let use to special matched image with options --image
*/
func CmpClientVersionAndClientImage(clientVersion, clientImgStr string) (bool, error) {
	clientImg, err := reference.ParseNormalizedNamed(clientImgStr)
	if err != nil {
		return false, err
	}
	clientImgTag, ok := clientImg.(reference.NamedTagged)
	if !ok {
		return false, fmt.Errorf("can not convert client image")
	}

	// 1. if client image version is match client cli version, does not need to upgrade
	// kubevpn connect --image=ghcr.io/kubenetworks/kubevpn:v2.3.0 or --kubevpnconfig
	// the kubevpn version is v2.3.1
	if IsVersionMajorOrMinorDiff(clientVersion, clientImgTag.Tag()) {
		// exit the process
		return true, nil
	}
	return false, nil
}

// CmpClientVersionAndPodImageTag version MAJOR.MINOR.PATCH
// if MAJOR or MINOR different, needs to upgrade
// otherwise not need upgrade
func CmpClientVersionAndPodImageTag(clientVersion string, serverImgStr string) bool {
	serverImg, err := reference.ParseNormalizedNamed(serverImgStr)
	if err != nil {
		return false
	}
	serverImgTag, ok := serverImg.(reference.NamedTagged)
	if !ok {
		return false
	}

	return IsVersionMajorOrMinorDiff(clientVersion, serverImgTag.Tag())
}

func IsVersionMajorOrMinorDiff(v1 string, v2 string) bool {
	version1, err := version.NewVersion(v1)
	if err != nil {
		return false
	}

	version2, err := version.NewVersion(v2)
	if err != nil {
		return false
	}
	if len(version1.Segments64()) != 3 {
		return false
	}
	if len(version2.Segments64()) != 3 {
		return false
	}
	if version1.Segments64()[0] != version2.Segments64()[0] {
		return true
	}
	if version1.Segments64()[1] != version2.Segments64()[1] {
		return true
	}
	return false
}

func GetTargetImage(version string, image string) string {
	serverImg, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return ""
	}
	serverImgTag, ok := serverImg.(reference.NamedTagged)
	if !ok {
		return ""
	}
	tag, err := reference.WithTag(serverImgTag, version)
	if err != nil {
		return ""
	}
	return tag.String()
}

// IsNewer
/**
version: MAJOR.MINOR.PATCH

MAJOR and MINOR different should be same, otherwise needs upgrade
*/
func IsNewer(clientVer string, clientImg string, serverImg string) (bool, error) {
	isNeedUpgrade, _ := CmpClientVersionAndClientImage(clientVer, clientImg)
	if isNeedUpgrade {
		err := errors.New("\n" + PrintStr(fmt.Sprintf("Current kubevpn cli version is %s, image is: %s, please use the same version of kubevpn image with flag \"--image\"", clientVer, clientImg)))
		return true, err
	}
	if CmpClientVersionAndPodImageTag(clientVer, serverImg) {
		return true, nil
	}
	return false, nil
}
