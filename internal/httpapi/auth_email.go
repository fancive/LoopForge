package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	emailCodeTTL      = 10 * time.Minute
	emailCodeCooldown = time.Minute
)

func canonicalEmail(raw string) string {
	email := strings.ToLower(strings.TrimSpace(raw))
	for _, char := range email {
		if char < 33 || char > 126 {
			return ""
		}
	}
	if len(email) > 254 || strings.Count(email, "@") != 1 {
		return ""
	}
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email {
		return ""
	}
	return email
}

func emailSignupEnabled() bool {
	return strings.TrimSpace(os.Getenv("AGENTFLOW_SIGNUP_EMAIL_DOMAINS")) != ""
}

func allowedSignupEmail(raw string) string {
	email := canonicalEmail(raw)
	if email == "" {
		return ""
	}
	_, domain, _ := strings.Cut(email, "@")
	for _, allowed := range strings.Split(os.Getenv("AGENTFLOW_SIGNUP_EMAIL_DOMAINS"), ",") {
		if strings.EqualFold(strings.TrimSpace(allowed), domain) {
			return email
		}
	}
	return ""
}

func (h *authHandlers) emailCodeDigest(email, code string) string {
	mac := hmac.New(sha256.New, []byte(h.getAuthClient().Config().SessionSecret))
	_, _ = fmt.Fprintf(mac, "email-registration\x00%s\x00%s", email, code)
	return hex.EncodeToString(mac.Sum(nil))
}

func (h *authHandlers) authEmailCode(c *gin.Context) {
	if !sameAuthOrigin(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "invalid request origin"})
		return
	}
	if !signupEnabled() || !emailSignupEnabled() || h.repo.LocalAccountCount() == 0 {
		c.JSON(http.StatusForbidden, gin.H{"error": "邮箱自助注册未开启"})
		return
	}
	if !h.allowPasswordAttempt(c.RemoteIP()) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "请求过于频繁，请稍后重试"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	var input struct {
		Email string `json:"email"`
	}
	if c.ShouldBindJSON(&input) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的邮箱请求"})
		return
	}
	email := allowedSignupEmail(input.Email)
	if email == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请输入允许注册的公司邮箱"})
		return
	}
	send := h.sendSignupEmail
	if send == nil {
		send = sendEmailRegistrationCode
	}
	if _, exists := h.repo.UserByVerifiedEmail(email); exists {
		c.Status(http.StatusAccepted)
		return
	}
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		respond(c, err)
		return
	}
	code := fmt.Sprintf("%06d", n.Int64())
	digest := h.emailCodeDigest(email, code)
	issued, err := h.repo.IssueEmailRegistration(email, digest, time.Now().UTC().Add(emailCodeTTL), emailCodeCooldown)
	if err != nil {
		respond(c, err)
		return
	}
	if issued {
		if err := send(c.Request.Context(), email, code); err != nil {
			if cleanupErr := h.repo.CancelEmailRegistration(email, digest); cleanupErr != nil {
				log.Printf("email registration: could not cancel unsent code")
			}
			log.Printf("email registration: SMTP delivery failed")
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "验证码暂时无法发送，请稍后重试"})
			return
		}
	}
	c.Status(http.StatusAccepted)
}

const signupEmailSubject = "LoopForge verification code"

func signupEmailBody(code string) string {
	return "Your LoopForge verification code is " + code + ". It expires in 10 minutes."
}

func sendEmailRegistrationCode(ctx context.Context, email, code string) error {
	endpoint := strings.TrimSpace(os.Getenv("AGENTFLOW_SIGNUP_MAIL_RELAY_URL"))
	token := strings.TrimSpace(os.Getenv("AGENTFLOW_SIGNUP_MAIL_RELAY_TOKEN"))
	if endpoint != "" || token != "" {
		client := signupRelayClient
		if socket := strings.TrimSpace(os.Getenv("AGENTFLOW_SIGNUP_MAIL_RELAY_SOCKET")); socket != "" {
			var cleanup func()
			var err error
			client, cleanup, err = signupRelayThroughSocket(socket)
			if err != nil {
				return err
			}
			defer cleanup()
		}
		return sendSignupCodeByRelay(ctx, client, endpoint, token, email, code)
	}
	addr := strings.TrimSpace(os.Getenv("AGENTFLOW_SIGNUP_SMTP_ADDR"))
	host, _, err := net.SplitHostPort(addr)
	from := canonicalEmail(os.Getenv("AGENTFLOW_SIGNUP_SMTP_FROM"))
	user := os.Getenv("AGENTFLOW_SIGNUP_SMTP_USER")
	password := os.Getenv("AGENTFLOW_SIGNUP_SMTP_PASSWORD")
	if err != nil || host == "" || from == "" || (user == "") != (password == "") {
		return errors.New("invalid signup SMTP configuration")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
		return err
	}
	if user != "" {
		if err := client.Auth(smtp.PlainAuth("", user, password, host)); err != nil {
			return err
		}
	}
	if err := client.Mail(from); err != nil {
		return err
	}
	if err := client.Rcpt(email); err != nil {
		return err
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	message := "From: " + from + "\r\nTo: " + email + "\r\nSubject: " + signupEmailSubject + "\r\n" +
		"Content-Type: text/plain; charset=UTF-8\r\n\r\n" + signupEmailBody(code) + "\r\n"
	if _, err := writer.Write([]byte(message)); err != nil {
		return err
	}
	return writer.Close()
}
