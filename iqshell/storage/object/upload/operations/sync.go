package operations

import (
	"github.com/qiniu/qshell/v2/iqshell"
	"github.com/qiniu/qshell/v2/iqshell/common/alert"
	"github.com/qiniu/qshell/v2/iqshell/common/data"
	"github.com/qiniu/qshell/v2/iqshell/common/log"
	"github.com/qiniu/qshell/v2/iqshell/common/progress"
	"github.com/qiniu/qshell/v2/iqshell/common/utils"
)

type SyncInfo UploadInfo

func (info *SyncInfo) Check() *data.CodeError {
	if len(info.FilePath) == 0 {
		return alert.CannotEmptyError("SrcResUrl", "")
	}
	if !utils.IsNetworkSource(info.FilePath) {
		return alert.Error("sync only for network source", "")
	}
	if len(info.ToBucket) == 0 {
		return alert.CannotEmptyError("Bucket", "")
	}
	if info.Overwrite && len(info.SaveKey) == 0 {
		return alert.CannotEmptyError("Overwrite mode and Key", "")
	}
	return nil
}

func SyncFile(cfg *iqshell.Config, info SyncInfo) {
	if shouldContinue := iqshell.CheckAndLoad(cfg, iqshell.CheckAndLoadInfo{
		Checker: &info,
	}); !shouldContinue {
		return
	}

	info.Progress = progress.NewPrintProgress(" 进度")
	ret, err := uploadFile((*UploadInfo)(&info))
	if err != nil {
		log.ErrorF("Sync file error %v", err)
	} else {
		log.Alert("")
		log.Alert("-------------- File FlowInfo --------------")
		log.AlertF("%10s%s", "Key: ", ret.Key)
		log.AlertF("%10s%s", "Hash: ", ret.ServerFileHash)
		log.AlertF("%10s%d%s", "Fsize: ", ret.ServerFileSize, "("+utils.FormatFileSize(ret.ServerFileSize)+")")
		log.AlertF("%10s%s", "MimeType: ", ret.MimeType)
	}
}
