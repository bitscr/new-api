package common

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// failingWriter 模拟客户端断开：写入直接失败。
type failingWriter struct {
	gin.ResponseWriter
}

func (f failingWriter) Write([]byte) (int, error)       { return 0, errors.New("client gone") }
func (f failingWriter) WriteString(string) (int, error) { return 0, errors.New("client gone") }

// 写入失败是"客户端在交付完成前断开"的硬证据；成功写完则不能标记。
// 只看 ctx.Err() 会把"客户端收完正文后正常关闭"也算成断开（实测导致所有
// auto 反馈被丢弃），所以这个信号必须精确。
func TestClientDeliveryBrokenOnlyTracksFailedWrites(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("写入失败则标记", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		info := &RelayInfo{answer: &answerObservation{}}
		SetRelayInfo(c, info)
		c.Writer = failingWriter{ResponseWriter: c.Writer}
		InstallRelayResponseWriter(c)

		if _, err := c.Writer.Write([]byte("data: {}\n\n")); err == nil {
			t.Fatal("失败的写入必须把错误抛出来")
		}
		if !info.ClientDeliveryBroken() {
			t.Fatal("写入失败后必须标记交付被截断")
		}
		if info.BeginAttempt(); info.ClientDeliveryBroken() {
			t.Fatal("新一次尝试必须清掉上一次的截断标记")
		}
	})

	t.Run("写入成功则保持干净", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		info := &RelayInfo{answer: &answerObservation{}}
		SetRelayInfo(c, info)
		InstallRelayResponseWriter(c)

		if _, err := c.Writer.Write([]byte("data: {}\n\n")); err != nil {
			t.Fatalf("正常写入不应报错: %v", err)
		}
		if info.ClientDeliveryBroken() {
			t.Fatal("写入成功不能标记为交付截断")
		}
	})

	t.Run("没有观察对象时不 panic", func(t *testing.T) {
		info := &RelayInfo{}
		info.MarkClientDeliveryBroken()
		if info.ClientDeliveryBroken() {
			t.Fatal("无观察对象时应恒为 false")
		}
	})
}
