//go:build zhenguojian

package sources

import "duanjuapp/native/core"

func init() {
	core.RegisterFlavor("zhenguojian", []string{
		sourceCloudFront,
		sourceHuangguoAI,
		sourceHuangguoVideo,
		sourceHuangju,
		sourceXduanju,
		sourceHuangtao,
	})
}
