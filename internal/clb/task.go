package clb

import (
	"context"
	"fmt"

	sdk "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/clb/v20180317"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// สถานะที่ DescribeTaskStatus คืนมา
const (
	taskSucceeded = 0
	taskFailed    = 1
	taskRunning   = 2
)

// waitTask รอ async task ของ CLB จนจบ
//
// ทุก mutating API ของ CLB เป็น async: มันคืน RequestId กลับมาทันที
// แต่ของจริงยังไม่เสร็จ ถ้ายิงคำสั่งถัดไปเลยจะเจอ error ตระกูล
// FailedOperation/IncorrectStatus เพราะ CLB ล็อกตัวเองอยู่
//
// error ระหว่าง poll ไม่ถือว่า fail — DescribeTaskStatus เองก็มี rate limit
// และ task ยังทำงานต่อไปอยู่ดี เรารอจนกว่าจะได้คำตอบชัดเจนหรือหมดเวลา
func (c *client) waitTask(ctx context.Context, requestID string) error {
	logger := log.FromContext(ctx).WithValues("taskId", requestID)

	err := wait.PollUntilContextTimeout(ctx, c.taskPollInterval, c.taskTimeout, true,
		func(ctx context.Context) (bool, error) {
			req := sdk.NewDescribeTaskStatusRequest()
			req.TaskId = common.StringPtr(requestID)

			if err := c.limiter.Wait(ctx); err != nil {
				return false, err
			}
			resp, err := c.clb.DescribeTaskStatusWithContext(ctx, req)
			if err != nil {
				// ถ้าเป็น terminal error (เช่น auth พัง) หยุดเลย ไม่ต้องรอหมดเวลา
				if cerr := classify(err); IsTerminal(cerr) {
					return false, cerr
				}
				logger.V(1).Info("polling task status failed, retrying", "err", err)
				return false, nil
			}
			if resp.Response == nil || resp.Response.Status == nil {
				return false, nil
			}

			switch *resp.Response.Status {
			case taskSucceeded:
				return true, nil
			case taskFailed:
				msg := ""
				if resp.Response.Message != nil {
					msg = *resp.Response.Message
				}
				// task ที่ CLB บอกว่า fail แล้ว retry คำสั่งเดิมมักไม่ช่วย
				return false, &TerminalError{Err: fmt.Errorf("clb task %s failed: %s", requestID, msg)}
			case taskRunning:
				return false, nil
			default:
				return false, nil
			}
		})

	if err != nil {
		if IsTerminal(err) {
			return err
		}
		return fmt.Errorf("waiting for clb task %s: %w", requestID, err)
	}
	return nil
}

// deref อ่านค่าจาก pointer ของ SDK อย่างปลอดภัย
func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
