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
func CmpClientVersionAndClientImage(clientVersion, clientImgStr string) (int, error) {
	clientImg, err := reference.ParseNormalizedNamed(clientImgStr)
	if err != nil {
		return 0, err
	}
	clientImgTag, ok := clientImg.(reference.NamedTagged)
	if !ok {
		return 0, fmt.Errorf("can not convert client image")
	}

	// 1. if client image version is match client cli version, does not need to upgrade
	// kubevpn connect --image=ghcr.io/kubenetworks/kubevpn:v2.3.0 or --kubevpnconfig
	// the kubevpn version is v2.3.1
	if cmp := CmpVersionMajorOrMinor(clientVersion, clientImgTag.Tag()); cmp != 0 {
		// exit the process
		return cmp, nil
	}
	return 0, nil
}

// CmpClientVersionAndPodImageTag version MAJOR.MINOR.PATCH
// if MAJOR or MINOR different, needs to upgrade
// otherwise not need upgrade
func CmpClientVersionAndPodImageTag(clientVersion string, serverImgStr string) int {
	serverImg, err := reference.ParseNormalizedNamed(serverImgStr)
	if err != nil {
		return 0
	}
	serverImgTag, ok := serverImg.(reference.NamedTagged)
	if !ok {
		return 0
	}

	return CmpVersionMajorOrMinor(clientVersion, serverImgTag.Tag())
}

func CmpVersionMajorOrMinor(v1 string, v2 string) int {
	version1, err := version.NewVersion(v1)
	if err != nil {
		return 0
	}

	version2, err := version.NewVersion(v2)
	if err != nil {
		return 0
	}
	if len(version1.Segments64()) != 3 {
		return 0
	}
	if len(version2.Segments64()) != 3 {
		return 0
	}
	if version1.Segments64()[0] > version2.Segments64()[0] {
		return 1
	} else if version1.Segments64()[0] < version2.Segments64()[0] {
		return -1
	}
	if version1.Segments64()[1] != version2.Segments64()[1] {
		return 1
	} else if version1.Segments64()[1] != version2.Segments64()[1] {
		return -1
	}
	return 0
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
	if isNeedUpgrade != 0 {
		err := errors.New("\n" + PrintStr(fmt.Sprintf("Current kubevpn cli version is %s, image is: %s, please use the same version of kubevpn image with flag \"--image\"", clientVer, clientImg)))
		return true, err
	}
	cmp := CmpClientVersionAndPodImageTag(clientVer, serverImg)
	if cmp > 0 {
		return true, nil
	} else if cmp < 0 {
		err := errors.New("\n" + PrintStr(fmt.Sprintf("Current kubevpn cli version is %s, image is: %s, please use the same version of kubevpn image with flag \"--image\"", clientVer, clientImg)))
		return true, err
	}
	return false, nil
}
