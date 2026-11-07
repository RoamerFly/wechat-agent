using System;
using System.Drawing;
using System.IO;
using System.Web;
using System.Windows.Forms;
using Microsoft.Web.WebView2.Core;
using Microsoft.Web.WebView2.WinForms;

namespace WechatAgent.Desktop {
    // This policy is deliberately independent of the UI.  The server remains
    // the final authority for the OAuth callback and exchanges the code only
    // after its own binding/state checks succeed.
    internal sealed class DouyinAuthorizationAttempt {
        internal readonly string State;
        internal readonly Uri CallbackURI;
        internal readonly DateTimeOffset ExpiresAt;
        internal DouyinAuthorizationAttempt(string state, Uri callbackURI, DateTimeOffset expiresAt) {
            State = state; CallbackURI = callbackURI; ExpiresAt = expiresAt;
        }
        internal bool IsExpired(DateTimeOffset now) { return now >= ExpiresAt; }
        internal bool AllowsNavigation(string value) {
            Uri uri;
            return Uri.TryCreate(value, UriKind.Absolute, out uri) && uri.Scheme == "https" && uri.Host.Equals("open.douyin.com", StringComparison.OrdinalIgnoreCase) &&
                uri.Port == 443 && uri.UserInfo == "" && uri.Fragment == "" &&
                (uri.AbsolutePath == "/platform/oauth/connect" || uri.AbsolutePath == "/platform/oauth/connect/" ||
                 uri.AbsolutePath == "/platform/oauth/pc/auth" || uri.AbsolutePath == "/platform/oauth/pc/auth/");
        }
        internal bool IsCallback(string value) {
            Uri uri;
            if (!Uri.TryCreate(value, UriKind.Absolute, out uri) || uri.UserInfo != "" || uri.Fragment != "" ||
                uri.Scheme != CallbackURI.Scheme || !uri.Host.Equals(CallbackURI.Host, StringComparison.OrdinalIgnoreCase) || uri.Port != CallbackURI.Port || uri.AbsolutePath != CallbackURI.AbsolutePath) return false;
            var query = HttpUtility.ParseQueryString(uri.Query);
            var state = query.GetValues("state");
            var code = query.GetValues("code");
            var scopes = query.GetValues("scopes");
            return state != null && state.Length == 1 && state[0] == State && code != null && code.Length == 1 && code[0].Length > 0 && scopes != null && scopes.Length == 1;
        }
    }

    internal static class DouyinAuthorizationPolicy {
        internal static string StateFromOfficialURL(string authorizationURL) {
            Uri uri;
            if (!Uri.TryCreate(authorizationURL, UriKind.Absolute, out uri) || uri.Scheme != "https" || uri.UserInfo != "" || uri.Fragment != "" ||
                !uri.Host.Equals("open.douyin.com", StringComparison.OrdinalIgnoreCase) || uri.Port != 443 ||
                (uri.AbsolutePath != "/platform/oauth/connect" && uri.AbsolutePath != "/platform/oauth/connect/")) return "";
            var states = HttpUtility.ParseQueryString(uri.Query).GetValues("state");
            if (states == null || states.Length != 1 || String.IsNullOrWhiteSpace(states[0]) || states[0].Length > 512) return "";
            foreach (char value in states[0]) if (Char.IsControl(value) || Char.IsWhiteSpace(value)) return "";
            return states[0];
        }
        internal static bool TryCreate(string authorizationURL, string expiresAt, DateTimeOffset now, out DouyinAuthorizationAttempt attempt) {
            attempt = null;
            Uri uri;
            if (!Uri.TryCreate(authorizationURL, UriKind.Absolute, out uri) || uri.Scheme != "https" || uri.UserInfo != "" || uri.Fragment != "" ||
                !uri.Host.Equals("open.douyin.com", StringComparison.OrdinalIgnoreCase) || uri.Port != 443 ||
                (uri.AbsolutePath != "/platform/oauth/connect" && uri.AbsolutePath != "/platform/oauth/connect/")) return false;
            var query = HttpUtility.ParseQueryString(uri.Query);
            var state = query.GetValues("state");
            var redirect = query.GetValues("redirect_uri");
            if (state == null || state.Length != 1 || StateFromOfficialURL(authorizationURL) == "" || redirect == null || redirect.Length != 1) return false;
            Uri callback;
            if (!Uri.TryCreate(redirect[0], UriKind.Absolute, out callback) || callback.Scheme != "https" || callback.Host == "" || callback.UserInfo != "" || callback.Query != "" || callback.Fragment != "") return false;
            DateTimeOffset serverExpiry;
            if (!DateTimeOffset.TryParse(expiresAt, out serverExpiry) || serverExpiry <= now) return false;
            var maximum = now.AddMinutes(10);
            attempt = new DouyinAuthorizationAttempt(state[0], callback, serverExpiry < maximum ? serverExpiry : maximum);
            return true;
        }
    }

    internal sealed class DouyinAuthorizationDialog : Form {
        readonly Deployment deployment;
        readonly DouyinAuthorizationAttempt attempt;
        readonly Action<string> completed;
        readonly Action<string> closed;
        readonly WebView2 view = new WebView2 { Dock = DockStyle.Fill, DefaultBackgroundColor = Color.White };
        readonly Label detail = new Label { Dock = DockStyle.Top, Height = 46, Padding = new Padding(20, 12, 12, 6), Text = "正在加载抖音授权二维码…" };
        readonly Timer expiration = new Timer { Interval = 1000 };
        bool terminal;

        internal DouyinAuthorizationDialog(Deployment deployment, string authorizationURL, DouyinAuthorizationAttempt attempt, Action<string> completed, Action<string> closed) {
            this.deployment = deployment; this.attempt = attempt; this.completed = completed; this.closed = closed;
            Text = "抖音扫码授权"; StartPosition = FormStartPosition.CenterParent; AutoScaleMode = AutoScaleMode.Dpi; Size = new Size(520, 700); MinimumSize = new Size(420, 560);
            FormBorderStyle = FormBorderStyle.Sizable; MaximizeBox = false; MinimizeBox = false; ShowInTaskbar = false; BackColor = Color.FromArgb(247, 249, 255);
            var cancel = new Button { Text = "取消", DialogResult = DialogResult.Cancel, Dock = DockStyle.Bottom, Height = 48, FlatStyle = FlatStyle.Flat, BackColor = Color.White };
            cancel.FlatAppearance.BorderColor = Color.FromArgb(206, 214, 234);
            cancel.Click += delegate { Finish(null, "已取消授权。"); };
            Controls.Add(view); Controls.Add(detail); Controls.Add(cancel); CancelButton = cancel;
            expiration.Tick += delegate { if (attempt.IsExpired(DateTimeOffset.UtcNow)) Finish(null, "授权二维码已过期，请重新生成。"); };
            Shown += async delegate {
                try {
                    Program.ConfigureWebViewLoader(deployment.Workspace);
                    var folder = Path.Combine(deployment.Workspace, "secrets", "douyin-oauth-webview");
                    Directory.CreateDirectory(folder);
                    // This is deliberately not the main page or the user\'s Edge profile.
                    var environment = await CoreWebView2Environment.CreateAsync(null, folder);
                    if (IsDisposed || terminal) return;
                    var options = environment.CreateCoreWebView2ControllerOptions();
                    options.IsInPrivateModeEnabled = true;
                    await view.EnsureCoreWebView2Async(environment, options);
                    if (IsDisposed || terminal) return;
                    Configure(); expiration.Start(); view.CoreWebView2.Navigate(authorizationURL);
                } catch (Exception) { Finish(null, "授权窗口未能启动，请重新生成二维码。"); }
            };
            FormClosed += delegate { expiration.Stop(); if (!terminal) Finish(null, "已取消授权。"); };
        }

        void Configure() {
            var core = view.CoreWebView2;
            core.Settings.AreDevToolsEnabled = false; core.Settings.AreDefaultContextMenusEnabled = false; core.Settings.AreDefaultScriptDialogsEnabled = false;
            core.Settings.AreHostObjectsAllowed = false; core.Settings.IsStatusBarEnabled = false; core.Settings.IsWebMessageEnabled = false;
            core.Settings.AreBrowserAcceleratorKeysEnabled = false; core.Settings.IsSwipeNavigationEnabled = false;
            view.AllowExternalDrop = false;
            core.IsMuted = true;
            core.NewWindowRequested += delegate(object sender, CoreWebView2NewWindowRequestedEventArgs e) { e.Handled = true; };
            core.DownloadStarting += delegate(object sender, CoreWebView2DownloadStartingEventArgs e) { e.Cancel = true; };
            core.PermissionRequested += delegate(object sender, CoreWebView2PermissionRequestedEventArgs e) { e.State = CoreWebView2PermissionState.Deny; };
            core.LaunchingExternalUriScheme += delegate(object sender, CoreWebView2LaunchingExternalUriSchemeEventArgs e) { e.Cancel = true; };
            core.ProcessFailed += delegate { Finish(null, "扫码窗口中断，请重新发起授权。"); };
            core.NavigationCompleted += delegate(object sender, CoreWebView2NavigationCompletedEventArgs e) {
                if (terminal) return;
                if (e.IsSuccess) detail.Text = "请用手机抖音扫码并确认授权。完成后返回成长站。";
                else if (e.WebErrorStatus != CoreWebView2WebErrorStatus.OperationCanceled)
                    Finish(null, "无法加载抖音授权页，请检查网络后重试。");
            };
            core.NavigationStarting += delegate(object sender, CoreWebView2NavigationStartingEventArgs e) {
                if (terminal) { e.Cancel = true; return; }
                if (attempt.IsCallback(e.Uri)) { e.Cancel = true; Finish(e.Uri, null); return; }
                if (!attempt.AllowsNavigation(e.Uri)) { e.Cancel = true; Finish(null, "授权窗口已阻止非授权页面。"); }
            };
        }

        protected override void Dispose(bool disposing) {
            if (disposing) expiration.Dispose();
            base.Dispose(disposing);
        }

        internal void CancelForServiceStop() { Finish(null, "服务已停止，授权窗口已关闭。"); }
        void Finish(string callback, string reason) {
            if (terminal) return;
            terminal = true; expiration.Stop();
            if (!String.IsNullOrEmpty(callback)) completed(callback); else closed(reason ?? "已取消授权。");
            if (IsHandleCreated && !IsDisposed) BeginInvoke(new Action(delegate { if (!IsDisposed) Close(); }));
        }
    }
}
