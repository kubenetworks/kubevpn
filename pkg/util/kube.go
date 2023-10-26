package util

import cmdutil "k8s.io/kubectl/pkg/cmd/util"

func GetKubeconfigPath(f cmdutil.Factory) string {
	rawConfig := f.ToRawKubeConfigLoader()
	if rawConfig.ConfigAccess().IsExplicitFile() {
		return rawConfig.ConfigAccess().GetExplicitFile()
	} else {
		return rawConfig.ConfigAccess().GetDefaultFilename()
	}
}
