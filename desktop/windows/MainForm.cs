using System;
using System.Collections.Generic;
using System.Diagnostics;
using System.Drawing;
using System.IO;
using System.Linq;
using System.Net;
using System.Threading;
using System.Threading.Tasks;
using System.Windows.Forms;
using Microsoft.Web.WebView2.Core;
using Microsoft.Web.WebView2.WinForms;

namespace WechatAgent.Desktop {
    internal sealed class MainForm : Form {
        readonly Deployment deployment;
        readonly ServiceController controller;
        readonly CloseSettings closeSettings;
        readonly EventWaitHandle activate;
        readonly NotifyIcon tray = new NotifyIcon();
        readonly ContextMenuStrip menu = new ContextMenuStrip();
        readonly ToolStripMenuItem startItem = new ToolStripMenuItem("启动全部服务"), stopItem = new ToolStripMenuItem("停止全部服务"), exitItem = new ToolStripMenuItem("退出并停止全部服务");
        readonly Label status = new Label();
        readonly Button retry = new Button(), cancel = new Button();
        readonly Panel placeholder = new Panel();
        readonly LifecycleSurface lifecycle = new LifecycleSurface();
        readonly System.Windows.Forms.Timer timer = new System.Windows.Forms.Timer();
        WebView2 web;
        DouyinAuthorizationDialog douyinAuthorization;
        CancellationTokenSource operation;
        bool busy, allowExit, exitRequested, stoppingCommitted;
        FormWindowState restoredState = FormWindowState.Maximized;
        bool closeDialogOpen;
        int pageGeneration;
        int webGeneration;
        bool closeQueued;
        EventWaitHandle startControl, stopControl, exitControl;

        internal MainForm(Deployment d, DesktopInstance instance) : this(d, instance.Activation) {
            startControl = instance.StartRequested; stopControl = instance.StopRequested; exitControl = instance.ExitRequested;
        }
        internal MainForm(Deployment d, EventWaitHandle activation) : this(d, activation, new ServiceController(d), true) { }
        internal MainForm(Deployment d, EventWaitHandle activation, ServiceController services, bool startOnShown) {
            deployment = d; activate = activation; controller = services;
            closeSettings = new CloseSettings(d.Workspace);
            Text = "Agent-home-station"; Size = new Size(1440, 980); MinimumSize = new Size(920, 640);
            WindowState = FormWindowState.Maximized;
            StartPosition = FormStartPosition.CenterScreen; AutoScaleMode = AutoScaleMode.Dpi;
            DoubleBuffered = true;
            Font = new Font("Microsoft YaHei UI", 11); BackColor = Color.White;
            Icon = Icon.ExtractAssociatedIcon(Application.ExecutablePath);
            // The management page owns the full client area. These controls are
            // visible only during startup, shutdown, or a recoverable failure.
            placeholder.Dock = DockStyle.Fill; placeholder.BackColor = Color.FromArgb(239, 243, 255);
            var center = new TableLayoutPanel { Dock = DockStyle.Fill, ColumnCount = 1, RowCount = 3, Padding = new Padding(0) };
            center.RowStyles.Add(new RowStyle(SizeType.Percent, 100)); center.RowStyles.Add(new RowStyle(SizeType.AutoSize)); center.RowStyles.Add(new RowStyle(SizeType.Absolute, 0));
            status.Text = "正在启动 Agent-home-station…"; status.Dock = DockStyle.Fill; status.TextAlign = ContentAlignment.BottomCenter; status.AutoSize = false;
            var buttons = new FlowLayoutPanel { AutoSize = true, Anchor = AnchorStyles.Top, WrapContents = false, Margin = new Padding(0, 24, 0, 0) };
            retry.Text = "重新启动"; retry.AccessibleName = retry.Text; retry.AutoSize = true; retry.MinimumSize = new Size(120, 44); retry.Visible = false;
            cancel.Text = "取消退出"; cancel.AccessibleName = cancel.Text; cancel.AutoSize = true; cancel.MinimumSize = new Size(120, 44); cancel.Visible = false;
            lifecycle.Dock=DockStyle.Fill;
            status.TextChanged+=delegate { lifecycle.Detail(status.Text); };
            buttons.Controls.AddRange(new Control[] {retry, cancel}); center.Controls.Add(lifecycle, 0, 0); center.Controls.Add(buttons, 0, 1);
            placeholder.Controls.Add(center); Controls.Add(placeholder);
            var show = new ToolStripMenuItem("打开管理页"); show.Click += delegate { RestoreWindow(); };
            var hide = new ToolStripMenuItem("收起到托盘（保持运行）"); hide.Click += delegate { ToTray(); };
            startItem.Click += async delegate { await StartServices(); };
            stopItem.Click += async delegate { await StopServices(false); };
            exitItem.Click += async delegate { RequestExit(); if (!busy) await StopServices(true); };
            var about = new ToolStripMenuItem("关于 Agent-home-station");
            var closeMode = new ToolStripMenuItem("关闭窗口时");
            foreach (var value in new [] { new [] {"ask", "每次询问"}, new [] {"tray", "最小化到托盘运行"}, new [] {"exit", "退出并停止全部服务"} }) {
                string behavior = value[0]; var item = new ToolStripMenuItem(value[1]) { Checked = closeSettings.Behavior == behavior };
                item.Click += delegate {
                    try { closeSettings.Save(behavior); foreach (ToolStripMenuItem other in closeMode.DropDownItems) other.Checked = other == item; }
                    catch (Exception error) { MessageBox.Show(this, "关闭方式未能保存，原设置保留。\n" + error.Message, "Agent-home-station", MessageBoxButtons.OK, MessageBoxIcon.Warning); }
                };
                closeMode.DropDownItems.Add(item);
            }
            closeMode.DropDownOpening += delegate { for (int i = 0; i < closeMode.DropDownItems.Count; i++) ((ToolStripMenuItem)closeMode.DropDownItems[i]).Checked = closeSettings.Behavior == new [] {"ask", "tray", "exit"}[i]; };
            about.Click += delegate { MessageBox.Show("Agent-home-station 桌面版 1.1.0\n开发者：RoamerFly\n项目：https://github.com/RoamerFly/Agent-home-station\n反馈：https://github.com/RoamerFly/Agent-home-station/issues\n\n最小化：保留任务栏窗口，继续运行\n关闭窗口：选择取消、托盘或退出，可记住选择\n选择退出：安全停止全部本实例服务并退出\n\n后端：" + (d.LocalStation ? "本机成长站（无需微信）" : d.Windows ? "Windows 原生（Linux 微信协议标识）" : "WSL") + "\n项目：" + d.Workspace + "\n服务数据：" + d.Root, "关于 Agent-home-station", MessageBoxButtons.OK, MessageBoxIcon.Information); };
            menu.Items.AddRange(new ToolStripItem[] {show, hide, new ToolStripSeparator(), startItem, stopItem, new ToolStripSeparator(), closeMode, about, exitItem});
            tray.Icon = Icon; tray.Text = "Agent-home-station · 正在启动"; tray.ContextMenuStrip = menu; tray.Visible = true;
            tray.DoubleClick += delegate { RestoreWindow(); };
            retry.Click += async delegate { await StartServices(); };
            cancel.Click += delegate { if (!stoppingCommitted && operation != null) { exitRequested = false; operation.Cancel(); } };
            controller.Progress = text => {
                status.Text = text;
                if (text.StartsWith("正在停止本实例")) { stoppingCommitted = true; cancel.Enabled = false; }
            };
            if (startOnShown) Shown += async delegate { await StartServices(); };
            Shown+=delegate { WindowEvent("shown"); };
            FormClosed+=delegate { WindowEvent("closed"); };
            FormClosing += async delegate(object sender, FormClosingEventArgs e) {
                if (allowExit) return; e.Cancel = true;
                if (closeDialogOpen || exitRequested) return;
                string choice = closeSettings.Behavior;
                if (choice == "ask") {
                    closeDialogOpen = true;
                    try {
                        bool dark = false;
                        if (web != null && web.CoreWebView2 != null) {
                            try {
                                var theme = web.ExecuteScriptAsync("document.documentElement.dataset.theme || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light')");
                                if (await Task.WhenAny(theme, Task.Delay(150)) == theme) dark = await theme == "\"dark\"";
                            } catch (InvalidOperationException) { }
                        }
                        if (exitRequested || IsDisposed) return;
                        using (var dialog = new CloseChoiceDialog(dark)) {
                            dialog.ShowDialog(this); choice = dialog.Choice;
                            if (choice == "cancel") return;
                            if (dialog.Remember) closeSettings.Save(choice);
                        }
                    } catch (Exception error) {
                        MessageBox.Show(this, "关闭方式未能保存，服务保持运行。\n" + error.Message, "Agent-home-station", MessageBoxButtons.OK, MessageBoxIcon.Warning); return;
                    } finally { closeDialogOpen = false; }
                }
                if (choice == "tray") { ToTray(); return; }
                RequestExit(); if (!busy) await StopServices(true);
            };
            timer.Interval = 250;
            timer.Tick += async delegate {
                if (activate.WaitOne(0)) RestoreWindow();
                if (exitControl != null && exitControl.WaitOne(0)) { RequestExit(); if (!busy) await StopServices(true); return; }
                if (busy || exitRequested) return;
                if (stopControl != null && stopControl.WaitOne(0)) await StopServices(false);
                else if (startControl != null && startControl.WaitOne(0)) await StartServices();
            }; timer.Start();
        }
        void RequestExit() {
            WindowEvent("exit_requested");
            exitRequested = true;
            ++pageGeneration;
            CloseDouyinAuthorization();
            if (busy) {
                status.Text = "操作完成后将安全停止服务并退出…";
                tray.Text = "Agent-home-station · 正在准备退出";
            }
        }
        internal void ToTray() {
            if (IsDisposed || Disposing || allowExit || exitRequested) return;
            // Hide alone removes the taskbar button. Changing ShowInTaskbar
            // recreates the native HWND and can leave a blank WebView host.
            Hide();
            WindowEvent("tray");
        }
        internal void RestoreWindow() {
            if (IsDisposed || Disposing || allowExit || exitRequested || closeQueued || closeDialogOpen) return;
            // Restore the state before Show, otherwise showing a minimized
            // form can fire Resize and immediately hide it in the tray again.
            if (WindowState == FormWindowState.Minimized) WindowState = restoredState;
            if (!Visible) Show();
            Activate();
            WindowEvent("activated");
        }
        void WindowEvent(string name) {
            try {
                string directory=Path.Combine(deployment.Workspace,"secrets"),path=Path.Combine(directory,"desktop-windows.jsonl");
                Directory.CreateDirectory(directory);
                if(File.Exists(path)&&new FileInfo(path).Length>65536)File.WriteAllText(path,"");
                File.AppendAllText(path,Deployment.Json.Serialize(new { time=DateTime.UtcNow, event_name=name, pid=Process.GetCurrentProcess().Id, executable=Application.ExecutablePath, main_window=IsHandleCreated?Handle.ToInt64():0, visible_management_windows=Application.OpenForms.Cast<Form>().Count(form=>form is MainForm&&form.Visible) })+Environment.NewLine);
            } catch(IOException) { } catch(UnauthorizedAccessException) { }
        }
        void ClosePage() {
            ++pageGeneration;
            CloseDouyinAuthorization();
            var view = web; web = null;
            if (view != null) { Controls.Remove(view); view.Dispose(); }
            if (!IsDisposed && !Disposing) { placeholder.Visible = true; placeholder.BringToFront(); }
        }
        void SetBusy(bool value, bool stopping) {
            busy = value; startItem.Enabled = stopItem.Enabled = !value;
            retry.Visible = false; cancel.Visible = value && stopping; cancel.Enabled = true;
        }
        internal async Task StartServices() {
            if (busy || closeQueued || allowExit) return; SetBusy(true, false); operation = new CancellationTokenSource();
            try {
                status.Text = "正在启动 Agent-home-station…"; tray.Text = "Agent-home-station · 正在启动";
                lifecycle.State("欢迎回家","正在准备大肥鱼的工作台…");
                await controller.Start(operation.Token);
                if (!exitRequested) await ShowPage();
                tray.Text = "Agent-home-station · 运行中";
            } catch (Exception error) { if (!IsDisposed && !Disposing) Error(error); }
            finally { operation.Dispose(); operation = null; if (!IsDisposed && !Disposing) { SetBusy(false, false); if (web == null) retry.Visible = true; } }
            if (exitRequested) await StopServices(true);
        }
        async Task StopServices(bool exiting) {
            if (busy || closeQueued || allowExit) return; if (exiting) exitRequested = true;
            SetBusy(true, true); stoppingCommitted = false; operation = new CancellationTokenSource();
            bool cancelled = false;
            bool completedExit = false;
            try {
                ++pageGeneration;CloseDouyinAuthorization();placeholder.Visible=true;placeholder.BringToFront();
                status.Text = "正在等待在途任务结束，随后停止全部服务…"; tray.Text = "Agent-home-station · 正在安全停止";
                lifecycle.State(exiting?"让大肥鱼休息一下":"正在收拾工作台",status.Text);
                await controller.Stop(operation.Token);
                ClosePage();lifecycle.State("已安全停止","登录、记忆与配置已保留。",false,true);
                tray.Text = "Agent-home-station · 已停止";
                completedExit = exitRequested;
            } catch (OperationCanceledException) {
                exitRequested = false; status.Text = "已取消退出，服务继续运行。"; tray.Text = "Agent-home-station · 运行中";
                cancelled = true;
            } catch (Exception error) { exitRequested = false; Error(error); }
            finally {
                operation.Dispose(); operation = null;
                if (!IsDisposed) { SetBusy(false, false); if (web == null) retry.Visible = true; }
            }
            if (completedExit) {
                // FormClosing is async: a synchronously completed Stop must
                // not recursively Close while the first close is cancelled.
                allowExit = true; closeQueued = true; timer.Stop(); tray.Visible = false;
                if(lifecycle.Animations)await Task.Delay(100);
                BeginInvoke(new Action(delegate { if (!IsDisposed && !Disposing) Close(); }));
                return;
            }
            if (cancelled) { if(web!=null){pageGeneration=webGeneration;placeholder.Visible=false;web.BringToFront();}else try { await ShowPage(); } catch (Exception error) { Error(error); } }
        }
        async Task ShowPage() {
            if (web != null || IsDisposed || Disposing || exitRequested || allowExit) return;
            int generation = webGeneration = ++pageGeneration;
            var view = new WebView2 { Dock = DockStyle.Fill, DefaultBackgroundColor = Color.White, Visible = false }; web = view; Controls.Add(view);
            try {
                Program.ConfigureWebViewLoader(deployment.Workspace);
                var environment = await CoreWebView2Environment.CreateAsync(null, Path.Combine(deployment.Workspace, "secrets", "desktop-webview"));
                if (!CurrentPage(view, generation)) return;
                await view.EnsureCoreWebView2Async(environment);
                if (!CurrentPage(view, generation)) return;
                // A hidden WebView can defer navigation indefinitely. Keep it
                // visible behind the startup panel until navigation finishes.
                view.Visible = true; placeholder.BringToFront();
                view.CoreWebView2.Settings.AreDevToolsEnabled = false;
                view.CoreWebView2.WebMessageReceived += delegate(object sender, CoreWebView2WebMessageReceivedEventArgs e) { HandleWebMessage(view, generation, e); };
                view.CoreWebView2.NewWindowRequested += delegate(object sender, CoreWebView2NewWindowRequestedEventArgs e) { e.Handled = true; if (!CurrentPage(view, generation)) return; if (Local(e.Uri)) view.CoreWebView2.Navigate(e.Uri); else if (e.IsUserInitiated && Http(e.Uri)) Process.Start(e.Uri); };
                view.CoreWebView2.NavigationStarting += delegate(object sender, CoreWebView2NavigationStartingEventArgs e) { if (!CurrentPage(view, generation)) { e.Cancel = true; return; } if (!Local(e.Uri) && e.Uri != "about:blank") { e.Cancel = true; if (e.IsUserInitiated && Http(e.Uri)) Process.Start(e.Uri); } };
                // Use protected API session cookies; never inject credentials or a host object into JS.
                await controller.Login(CancellationToken.None);
                if (!CurrentPage(view, generation)) return;
                foreach (Cookie cookie in controller.SessionCookies()) {
                    var copy = view.CoreWebView2.CookieManager.CreateCookie(cookie.Name, cookie.Value, cookie.Domain, cookie.Path);
                    copy.IsHttpOnly = cookie.HttpOnly; copy.IsSecure = cookie.Secure;
                    view.CoreWebView2.CookieManager.AddOrUpdateCookie(copy);
                }
                view.CoreWebView2.NavigationCompleted += delegate(object sender, CoreWebView2NavigationCompletedEventArgs e) {
                    if (!CurrentPage(view, generation)) return;
                    if(!e.IsSuccess){Error(new InvalidOperationException("管理页加载失败，请重试启动。"));return;}
                    view.Visible = true; view.BringToFront(); placeholder.Visible = false;
                };
                view.CoreWebView2.Navigate(deployment.Url + "/");
            } catch { if (CurrentPage(view, generation)) { ClosePage(); throw; } }
        }
        void HandleWebMessage(WebView2 view, int generation, CoreWebView2WebMessageReceivedEventArgs message) {
            if (!CurrentPage(view, generation) || !deployment.LocalStation || !ManagementPage(message.Source)) return;
            Dictionary<string, object> input;
            try { input = Deployment.Json.Deserialize<Dictionary<string, object>>(message.WebMessageAsJson); }
            catch (Exception) { return; }
            if (input == null || Deployment.Text(input, "type") != "douyin-auth-open") return;
            var authorizationURL = Deployment.Text(input, "url");
            var expiresAt = Deployment.Text(input, "expires_at");
            DouyinAuthorizationAttempt attempt;
            if (!DouyinAuthorizationPolicy.TryCreate(authorizationURL, expiresAt, DateTimeOffset.UtcNow, out attempt)) {
                PostDouyinAuthorization(view, generation, "douyin-auth-closed", DouyinAuthorizationPolicy.StateFromOfficialURL(authorizationURL), "", "授权信息无效或已过期，请重新生成二维码。");
                return;
            }
            CloseDouyinAuthorization();
            var dialog = new DouyinAuthorizationDialog(deployment, authorizationURL, attempt,
                callback => PostDouyinAuthorization(view, generation, "douyin-auth-callback", attempt.State, callback, ""),
                reason => PostDouyinAuthorization(view, generation, "douyin-auth-closed", attempt.State, "", reason));
            douyinAuthorization = dialog;
            dialog.FormClosed += delegate { if (douyinAuthorization == dialog) douyinAuthorization = null; };
            dialog.Show(this);
        }
        void PostDouyinAuthorization(WebView2 view, int generation, string type, string state, string callback, string error) {
            if (!CurrentPage(view, generation) || view.CoreWebView2 == null || !ManagementPage(view.Source == null ? "" : view.Source.ToString())) return;
            try { view.CoreWebView2.PostWebMessageAsJson(Deployment.Json.Serialize(new { type = type, state = state, callback_url = callback, error = error })); }
            catch (InvalidOperationException) { }
        }
        void CloseDouyinAuthorization() {
            var dialog = douyinAuthorization; douyinAuthorization = null;
            if (dialog != null && !dialog.IsDisposed) dialog.CancelForServiceStop();
        }
        bool CurrentPage(WebView2 view, int generation) {
            return !IsDisposed && !Disposing && !exitRequested && !allowExit && generation == pageGeneration && web == view && !view.IsDisposed;
        }
        bool ManagementPage(string url) {
            Uri value, expected;
            return Uri.TryCreate(url, UriKind.Absolute, out value) && Uri.TryCreate(deployment.Url, UriKind.Absolute, out expected) &&
                value.Scheme == expected.Scheme && value.Host == expected.Host && value.Port == expected.Port && value.UserInfo == "" && value.AbsolutePath == "/" && value.Query == "" && value.Fragment == "";
        }
        protected override void WndProc(ref Message message) {
            base.WndProc(ref message);
            // WM_SIZE carries the committed native state. Resize can run
            // before Form.WindowState updates and otherwise saves stale Max.
            if (message.Msg == 0x0005 && !allowExit) {
                if (message.WParam.ToInt64() == 0) restoredState = FormWindowState.Normal;
                else if (message.WParam.ToInt64() == 2) restoredState = FormWindowState.Maximized;
            }
        }
        bool Local(string url) { Uri uri; return Uri.TryCreate(url, UriKind.Absolute, out uri) && uri.GetLeftPart(UriPartial.Authority) == deployment.Url && uri.UserInfo == ""; }
        static bool Http(string url) { Uri uri; return Uri.TryCreate(url, UriKind.Absolute, out uri) && (uri.Scheme == "http" || uri.Scheme == "https"); }
        void Error(Exception error) { if (IsDisposed || Disposing || allowExit) return; ClosePage(); status.Text = error.Message;lifecycle.State("操作未完成",error.Message,false); retry.Visible = true; tray.Text = "Agent-home-station · 需要处理"; RestoreWindow(); }
        protected override void Dispose(bool disposing) {
            if (disposing) { ++pageGeneration; allowExit = true; CloseDouyinAuthorization(); timer.Dispose(); tray.Visible = false; tray.Dispose(); menu.Dispose(); if (web != null) { web.Dispose(); web = null; } controller.Dispose(); }
            base.Dispose(disposing);
        }
    }
}
