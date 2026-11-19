document.getElementById('loginLang').onclick = () => window.lfSetLang(window.lfLang() === 'en' ? 'zh' : 'en');
document.getElementById('loginTheme').onclick = () => {
  const next = document.documentElement.dataset.theme === 'light' ? 'dark' : 'light';
  document.documentElement.dataset.theme = next;
  localStorage.setItem('agentflow.theme.v3', next);
};

const $ = id => document.getElementById(id);
const t = window.lfT || ((zh) => zh);
let mode = 'login', signup = false, emailSignup = false;

function render() {
  const creating = mode !== 'login';
  $('loginSubtitle').textContent = mode === 'setup' ? t('创建管理员账号', 'Create the administrator account') : creating ? t('创建你的账号', 'Create your account') : t('登录你的交付工作台', 'Sign in to your delivery workspace');
  const form = $('loginForm'), verifyEmail = mode === 'register' && emailSignup;
  $('loginNameLabel').textContent = creating ? t('用户名', 'Username') : t('用户名或邮箱', 'Username or email');
  $('loginNameField').hidden = verifyEmail;
  form.elements.login.required = !verifyEmail;
  form.elements.login.disabled = verifyEmail;
  form.elements.login.maxLength = creating ? 64 : 254;
  $('signupEmailField').hidden = $('signupCodeField').hidden = $('signupSendCode').hidden = $('signupCodeStatus').hidden = !verifyEmail;
  form.elements.email.required = form.elements.code.required = verifyEmail;
  form.elements.email.disabled = form.elements.code.disabled = !verifyEmail;
  $('signupEmailLabel').textContent = t('公司邮箱', 'Company email');
  $('signupCodeLabel').textContent = t('邮箱验证码', 'Email verification code');
  $('signupSendCode').textContent = t('发送验证码', 'Send code');
  $('signupCodeStatus').textContent = '';
  form.elements.code.value = '';
  $('setupNotice').hidden = mode !== 'setup';
  $('confirmField').hidden = !creating;
  $('passwordRule').hidden = !creating;
  $('loginForm').elements.confirm.required = creating;
  $('loginForm').elements.password.autocomplete = creating ? 'new-password' : 'current-password';
  $('loginSubmit').textContent = mode === 'setup' ? t('创建并进入', 'Create and continue') : creating ? t('注册并进入', 'Sign up') : t('登录', 'Sign in');
  $('loginSwitch').hidden = mode === 'setup' || !signup;
  $('switchText').textContent = creating ? t('已有账号？', 'Already have an account?') : t('还没有账号？', 'No account yet?');
  $('switchMode').textContent = creating ? t('登录', 'Sign in') : t('注册', 'Sign up');
  $('loginContact').hidden = mode === 'setup' || signup;
  $('loginError').textContent = '';
}

$('switchMode').onclick = () => { mode = mode === 'login' ? 'register' : 'login'; render(); };

fetch('/api/auth/setup').then(r => r.ok ? r.json() : {}).then(state => {
  signup = !!state.signupEnabled;
  emailSignup = !!state.emailSignup;
  if (state.needsSetup) mode = 'setup';
  else if (location.hash === '#register' && signup) mode = 'register';
  render();
}).catch(() => {});

$('signupSendCode').onclick = async () => {
  const email = $('loginForm').elements.email, button = $('signupSendCode'), status = $('signupCodeStatus');
  if (!email.reportValidity()) return;
  button.disabled = true;
  status.textContent = '';
  try {
    const response = await fetch('/api/auth/email-code', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({email: email.value.trim()}),
    });
    if (!response.ok) {
      const payload = await response.json();
      throw new Error(payload.error || t('验证码发送失败', 'Could not send the code'));
    }
    status.textContent = t('请查收公司邮箱。已有有效验证码时请使用最近收到的验证码。', 'Check your company inbox and use the most recent code.');
  } catch (err) { status.textContent = err.message; }
  finally { button.disabled = false; }
};

$('loginForm').onsubmit = async event => {
  event.preventDefault();
  const form = event.currentTarget, button = $('loginSubmit'), error = $('loginError');
  const creating = mode !== 'login';
  if (creating && form.elements.password.value !== form.elements.confirm.value) {
    error.textContent = t('两次输入的密码不一致', 'Passwords do not match');
    return;
  }
  button.disabled = true;
  error.textContent = '';
  try {
    const response = await fetch(creating ? '/api/auth/register' : '/api/auth/password', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({
        password: form.elements.password.value,
        ...(mode === 'register' && emailSignup
          ? {email: form.elements.email.value.trim(), code: form.elements.code.value}
          : {login: form.elements.login.value.trim()}),
      }),
    });
    const payload = await response.json();
    if (!response.ok) throw new Error(payload.error || t('操作失败', 'Request failed'));
    location.replace('/');
  } catch (err) { error.textContent = err.message; button.disabled = false; }
};
