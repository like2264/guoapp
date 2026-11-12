//go:build !zhenguojian

package sources

import "duanjuapp/native/core"

func init() {
	core.RegisterFlavor("hongguojian", []string{
		sourceCloudFront,
		sourceHuangguoAI,
		sourceHuangguoVideo,
		sourceHuangju,
		sourceXduanju,
		sourceHuangtao,
	})
}
