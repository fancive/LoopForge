package httpapi

import (
	"agentflow/internal/domain"
	"agentflow/internal/repo"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

var (
	registerMu       sync.Mutex
	emailCodePattern = regexp.MustCompile(`^[0-9]{6}$`)
	loginPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{1,63}$`)
)

// signupEnabled reports whether visitors may create accounts after the first
// administrator exists. Defaults to on; public deployments should disable it.
func signupEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AGENTFLOW_ALLOW_SIGNUP"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

func (h *authHandlers) authSetup(c *gin.Context) {
	needsSetup := h.repo != nil && h.repo.LocalAccountCount() == 0
	state := gin.H{"needsSetup": needsSetup, "signupEnabled": needsSetup || signupEnabled()}
	if emailSignupEnabled() {
		state["emailSignup"] = !needsSetup && signupEnabled() && emailSignupEnabled()
	}
	c.JSON(http.StatusOK, state)
}

const welcomeProjectID = "welcome"

// joinWelcomeProject makes the seeded demo project usable by new accounts so
// the onboarding path works before they create a project of their own.
func (h *authHandlers) joinWelcomeProject(userID int64) { joinWelcomeProject(h.repo, userID) }

func joinWelcomeProject(store *repo.Store, userID int64) {
	project, ok := store.Project(welcomeProjectID)
	if !ok {
		return
	}
	for _, m := range project.Members {
		if m.UserID == userID {
			return
		}
	}
	project.Members = append(project.Members, domain.ProjectMember{UserID: userID, Role: "member"})
	if err := store.PutProject(project); err != nil {
		log.Printf("join welcome project: %v", err)
	}
}

func sameAuthOrigin(c *gin.Context) bool {
	origin, err := url.Parse(c.GetHeader("Origin"))
	return err == nil && origin.Host == c.Request.Host && (origin.Scheme == "http" || origin.Scheme == "https")
}

func (h *authHandlers) authRegister(c *gin.Context) {
	if !sameAuthOrigin(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "invalid request origin"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	var input struct {
		Login    string `json:"login"`
		Password string `json:"password"`
		Email    string `json:"email"`
		Code     string `json:"code"`
	}
	if c.ShouldBindJSON(&input) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid registration request"})
		return
	}
	input.Login = strings.TrimSpace(input.Login)
	if len(input.Password) < repo.MinPasswordLength || len(input.Password) > repo.MaxPasswordLength {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("密码需 %d–%d 个字符", repo.MinPasswordLength, repo.MaxPasswordLength)})
		return
	}
	registerMu.Lock()
	first := h.repo.LocalAccountCount() == 0
	verifyEmail := !first && emailSignupEnabled()
	var email string
	if verifyEmail {
		email = allowedSignupEmail(input.Email)
		if email == "" || !emailCodePattern.MatchString(input.Code) {
			registerMu.Unlock()
			c.JSON(http.StatusBadRequest, gin.H{"error": "请输入允许注册的公司邮箱和六位验证码"})
			return
		}
	} else if !loginPattern.MatchString(input.Login) {
		registerMu.Unlock()
		c.JSON(http.StatusBadRequest, gin.H{"error": "用户名需 2–64 位，以字母或数字开头，只能包含字母、数字、_ . -"})
		return
	}
	// Only a valid request reaches account creation and spends the shared
	// budget (see passwordLimiter).
	if !h.allowPasswordAttempt(c.RemoteIP()) {
		registerMu.Unlock()
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "Too many attempts. Try again in a minute."})
		return
	}
	if !first && !signupEnabled() {
		registerMu.Unlock()
		c.JSON(http.StatusForbidden, gin.H{"error": "自助注册已关闭，请联系管理员创建账号"})
		return
	}
	var u domain.User
	var err error
	if verifyEmail {
		u, err = h.repo.CreateVerifiedLocalAccount(email, input.Password, h.emailCodeDigest(email, input.Code), time.Now().UTC())
	} else {
		u, err = h.repo.CreateLocalAccount(input.Login, input.Password, first)
	}
	if err == nil && !first {
		h.joinWelcomeProject(u.ID)
	}
	registerMu.Unlock()
	if errors.Is(err, repo.ErrEmailCodeInvalid) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "验证码无效、已使用或已过期"})
		return
	}
	if errors.Is(err, repo.ErrLoginTaken) {
		message := "该用户名已被使用"
		if verifyEmail {
			message = "该邮箱已注册，请登录"
		}
		c.JSON(http.StatusConflict, gin.H{"error": message})
		return
	}
	if err != nil {
		respond(c, err)
		return
	}
	user := domainUserToAuth(u)
	token, err := h.getAuthClient().SignUser(user)
	if err != nil {
		respond(c, err)
		return
	}
	h.setAuthCookie(c.Writer, c.Request, token)
	c.JSON(http.StatusOK, gin.H{"user": h.annotateUser(user), "admin": first})
}
