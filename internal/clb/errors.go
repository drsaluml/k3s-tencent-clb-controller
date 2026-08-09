package clb

import (
	"errors"
	"strings"

	sdkerrors "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/errors"
)

// retryablePrefixes คือ error code ที่ "ลองใหม่แล้วมีโอกาสสำเร็จ"
//
// FailedOperation / IncorrectStatus เกิดตอน CLB ยังล็อกตัวเองจาก task ก่อนหน้า
// RequestLimitExceeded คือชน rate limit ฝั่ง Tencent
var retryablePrefixes = []string{
	"RequestLimitExceeded",
	"InternalError",
	"FailedOperation",
	"IncorrectStatus",
	"ResourceInsufficient",
	"UnsupportedOperation.ResourceInOperating",
}

// terminalPrefixes คือ error ที่ retry ไปก็เท่านั้น — ต้องให้คนแก้
// สำคัญมาก: ถ้า retry error พวกนี้จะกลายเป็น hot loop เผา API quota
// และกลบ error จริงจนหาไม่เจอ
var terminalPrefixes = []string{
	"AuthFailure",
	"UnauthorizedOperation",
	"InvalidParameter",
	"InvalidParameterValue",
	"MissingParameter",
	"LimitExceeded", // quota เต็ม — retry ไม่ช่วย ต้องขอเพิ่ม
}

// TerminalError ห่อ error ที่ไม่ควร retry
// controller-runtime จะเห็นเป็น error ปกติ แต่ reconciler เช็ค IsTerminal
// แล้วเลือก backoff ยาวแทนที่จะ retry ทันที
type TerminalError struct{ Err error }

func (e *TerminalError) Error() string { return e.Err.Error() }
func (e *TerminalError) Unwrap() error { return e.Err }

// IsTerminal บอกว่า error นี้ retry ไปก็ไม่หาย
func IsTerminal(err error) bool {
	var te *TerminalError
	return errors.As(err, &te)
}

// classify แปลง SDK error เป็น TerminalError ถ้าเข้าข่าย
// error ที่ไม่รู้จัก (เช่น network timeout) ถือว่า retryable ไว้ก่อน
func classify(err error) error {
	if err == nil {
		return nil
	}
	var sdkErr *sdkerrors.TencentCloudSDKError
	if !errors.As(err, &sdkErr) {
		return err // network/transport error → retry ได้
	}
	code := sdkErr.GetCode()
	for _, p := range retryablePrefixes {
		if strings.HasPrefix(code, p) {
			return err
		}
	}
	for _, p := range terminalPrefixes {
		if strings.HasPrefix(code, p) {
			return &TerminalError{Err: err}
		}
	}
	return err
}
