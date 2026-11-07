using System;
using System.Collections;
using System.Collections.Generic;
using System.Diagnostics;
using System.Drawing;
using System.IO;
using System.Linq;
using System.Net;
using System.Net.Http;
using System.Net.Sockets;
using System.Reflection;
using System.Threading;
using System.Threading.Tasks;
using System.Text;
using System.Windows.Forms;
using Microsoft.Web.WebView2.Core;
using Microsoft.Web.WebView2.WinForms;

namespace WechatAgent.Desktop {
    internal static class WindowTests {
        sealed class SyntheticPageServer : IDisposable {
            readonly TcpListener listener = new TcpListener(IPAddress.Loopback, 0);
            internal string Url { get; private set; }
            internal int Requests;
            internal SyntheticPageServer() {
                listener.Start(); Url = "http://127.0.0.1:" + ((IPEndPoint)listener.LocalEndpoint).Port;
                Serve();
            }
            async void Serve() {
                try {
                    while (true) Reply(await listener.AcceptTcpClientAsync());
                } catch (ObjectDisposedException) { } catch (SocketException) { }
            }
            async void Reply(TcpClient client) {
                try {
                    using (client) using (var stream = client.GetStream()) {
                        var request = new byte[4096];
                        var read = stream.ReadAsync(request, 0, request.Length);
                        if (await Task.WhenAny(read, Task.Delay(2000)) != read || await read == 0) return;
                        ++Requests;
                        byte[] body = Encoding.UTF8.GetBytes("<!doctype html><title>Synthetic desktop lifecycle</title><p>Safe synthetic management page</p>");
                        byte[] header = Encoding.ASCII.GetBytes("HTTP/1.1 200 OK\r\nContent-Type: text/html; charset=utf-8\r\nContent-Length: " + body.Length + "\r\nConnection: close\r\n\r\n");
                        await stream.WriteAsync(header, 0, header.Length); await stream.WriteAsync(body, 0, body.Length);
                    }
                } catch (ObjectDisposedException) { } catch (SocketException) { } catch (IOException) { }
            }
            public void Dispose() { listener.Stop(); }
        }
        static int assertions;
        static void LoadingSurfaceTests(string root) {
            foreach (var size in new[] { new Size(320,240),new Size(640,480),new Size(1000,720) }) {
                using(var surface=new LifecycleSurface { Size=size,Font=new Font("Microsoft YaHei UI",11) })
                using(var bitmap=new Bitmap(size.Width,size.Height)) {
                    surface.State("正在启动", "正在准备本机服务…");surface.CreateControl();
                    surface.DrawToBitmap(bitmap,new Rectangle(Point.Empty,size));
                    int blue=0;bool clipped=false;
                    for(int y=0;y<size.Height;y++)for(int x=0;x<size.Width;x++) {
                        var c=bitmap.GetPixel(x,y);
                        if(c.B>100&&c.R<150&&c.G<150){blue++;if(x<3||y<3||x>=size.Width-3||y>=size.Height-3)clipped=true;}
                    }
                    Assert(blue>100&&!clipped,"home emblem must be fully inside its surface");
                    bitmap.Save(Path.Combine(root,"loading-"+size.Width+".png"));
                }
            }
        }
        static void Assert(bool value, string reason) { if (!value) throw new Exception(reason); assertions++; }
        static async Task Wait(Func<bool> condition, string reason) {
            for (int i = 0; i < 150; ++i) { if (condition()) return; await Task.Delay(100); }
            throw new Exception(reason);
        }
        static Process Child(params string[] args) {
            var info = HiddenRunner.Info(Application.ExecutablePath, args);
            return Process.Start(info);
        }
        static async Task InstanceTests(string root) {
            string name = "Local\\WechatAgentSynthetic-" + Guid.NewGuid().ToString("N");
            using (var owner = new DesktopInstance(name)) {
                Assert(owner.IsOwner, "first process owns instance");
                var children = Enumerable.Range(0, 12).Select(i => Child("--instance-probe", name)).ToArray();
                foreach (var child in children) using (child) {
                    string result = await child.StandardOutput.ReadToEndAsync();
                    await Wait(() => child.HasExited, "second process did not exit");
                    Assert(child.ExitCode == 0 && result.Trim() == "secondary", "simultaneous duplicate acquired ownership");
                }
                Assert(owner.Activation.WaitOne(0), "duplicate signals the existing window");
            }
            string ready = Path.Combine(root, "abandoned-owner-ready");
            using (var abandoned = Child("--instance-hold", name, ready)) {
                await Wait(() => File.Exists(ready), "owner probe did not acquire");
                abandoned.Kill(); abandoned.WaitForExit();
            }
            using (var recovered = new DesktopInstance(name)) Assert(recovered.IsOwner, "abandoned instance is recoverable");
        }
        static void CheckChoiceLayout(Control parent) {
            foreach (Control control in parent.Controls) {
                Assert(parent.ClientRectangle.Contains(control.Bounds), "dialog content clipped: " + control.Text);
                if (control is Button || control is CheckBox || control is Label) {
                    var size = TextRenderer.MeasureText(control.Text, control.Font,
                        new Size(control.ClientSize.Width, int.MaxValue), TextFormatFlags.WordBreak);
                    Assert(size.Height <= control.ClientSize.Height, "dialog text height clipped: " + control.Text);
                    if (control is Button) Assert(TextRenderer.MeasureText(control.Text, control.Font).Width <= control.ClientSize.Width - 6, "dialog button text clipped");
                }
                CheckChoiceLayout(control);
            }
        }
        static async Task FormTests(string root) {
            using (var owner = new Form { Bounds = new Rectangle(80, 60, 800, 600), StartPosition = FormStartPosition.Manual })
            using (var choice = new CloseChoiceDialog()) {
                owner.Show();
                Exception layoutError = null;
                choice.Shown += delegate {
                    try {
                    var b = owner.Bounds;
                    var area = Screen.FromControl(owner).WorkingArea;
                    int x = Math.Max(area.Left, Math.Min(area.Right-choice.Width, b.Left+(b.Width-choice.Width)/2));
                    int y = Math.Max(area.Top, Math.Min(area.Bottom-choice.Height, b.Top+(b.Height-choice.Height)/2));
                    Assert(choice.Location == new Point(x,y), "close choice centered on owner");
                    File.WriteAllText(Path.Combine(root,"close-choice-layout.txt"), "DPI=" + choice.DeviceDpi + "; font=" + choice.Font.Size + "; size=" + choice.ClientSize + "; scale=" + choice.AutoScaleDimensions);
                    CheckChoiceLayout(choice);
                    using (var bitmap = new Bitmap(choice.Width, choice.Height)) {
                        choice.DrawToBitmap(bitmap, new Rectangle(Point.Empty, bitmap.Size));
                        bitmap.Save(Path.Combine(root, "close-choice.png"));
                    }
                    } catch (Exception error) { layoutError = error; }
                    finally { choice.Close(); }
                };
                choice.ShowDialog(owner);
                if (layoutError != null) throw layoutError;
                Assert(choice.Choice == "cancel", "dismissing close choice preserves services");
            }
            using (var page = new SyntheticPageServer()) {
            for (int cycle = 0; cycle < 8; ++cycle) {
                string workspace = Path.Combine(root, "window-" + cycle); Directory.CreateDirectory(workspace);
                new CloseSettings(workspace).Save(cycle == 0 ? "tray" : "exit");
                var d = new Deployment { Workspace = workspace, Distro = "Synthetic", User = "synthetic", Root = "/home/synthetic/data", Account = "synthetic", Music = "", Url = page.Url };
                var commands = new FakeCommands(); var api = new FakeWeb();
                using (var activation = new EventWaitHandle(false, EventResetMode.AutoReset))
                using (var services = new ServiceController(d, commands, api, (span, token) => Task.FromResult(0)))
                using (var form = new MainForm(d, activation, services, false)) {
                    Assert(form.WindowState == FormWindowState.Maximized, "initial window is maximized");
                    form.WindowState = FormWindowState.Normal; form.StartPosition = FormStartPosition.Manual;
                    form.Location = new Point(-20000, -20000); form.Show();
                    await Task.Delay(100);
                    form.WindowState = FormWindowState.Normal;
                    await Task.Delay(100);
                    IntPtr handle = form.Handle;
                    form.WindowState = FormWindowState.Minimized;
                    Assert(form.Visible && form.ShowInTaskbar, "normal minimize retains taskbar window");
                    form.RestoreWindow();
                    Assert(form.Visible && form.WindowState == FormWindowState.Normal, "minimize restore preserves prior state: " + form.WindowState + ", visible=" + form.Visible);
                    form.ToTray(); Assert(!form.Visible, "explicit tray hides window");
                    Assert(form.Handle == handle, "tray must not recreate HWND");
                    form.RestoreWindow(); Assert(form.Visible && form.Handle == handle, "restore reuses original HWND");
                    if (cycle == 0) {
                        form.Close(); Assert(!form.Visible && !form.IsDisposed && !commands.Stopped, "remembered tray close preserves service");
                        form.RestoreWindow();
                        var exit = (ToolStripMenuItem)typeof(MainForm).GetField("exitItem", BindingFlags.Instance | BindingFlags.NonPublic).GetValue(form);
                        exit.PerformClick();
                    } else {
                        Task starting = form.StartServices();
                        if (cycle % 2 == 0) {
                            await starting; // alternate loaded page and initialization race
                            try {
                                await Wait(() => form.Controls.OfType<WebView2>().Any(view => view.CoreWebView2 != null && view.Visible && view.CoreWebView2.DocumentTitle == "Synthetic desktop lifecycle"), "synthetic management page did not render");
                            } catch {
                                var status = (Label)typeof(MainForm).GetField("status", BindingFlags.NonPublic | BindingFlags.Instance).GetValue(form);
                                Console.Error.WriteLine("Page diagnostic: " + status.Text + "; views=" + form.Controls.OfType<WebView2>().Count());
                                foreach (var view in form.Controls.OfType<WebView2>()) Console.Error.WriteLine("cycle=" + cycle + "; source=" + view.Source + "; visible=" + view.Visible + "; title=" + (view.CoreWebView2 == null ? "uninitialized" : view.CoreWebView2.DocumentTitle) + "; requests=" + page.Requests);
                                throw;
                            }
                            Assert(true, "real WebView rendered isolated synthetic page");
                        }
                        form.Close();
                        activation.Set(); form.RestoreWindow(); // duplicate-launch race during exit
                        await starting;
                    }
                    await Wait(() => form.IsDisposed, "exit left an empty management window");
                    form.RestoreWindow(); form.ToTray();
                    typeof(MainForm).GetMethod("Error", BindingFlags.Instance | BindingFlags.NonPublic).Invoke(form, new object[] {new Exception("late synthetic page callback")});
                    await Task.Delay(150);
                    Assert(!Application.OpenForms.Cast<Form>().Any(f => f is MainForm), "late callback resurrected management window");
                    Assert(commands.Stopped, "exit stops owned services");
                }
            }
            }
        }
        static string FreeLocalUrl() {
            var listener = new TcpListener(IPAddress.Loopback, 0);
            try {
                listener.Start();
                return "http://127.0.0.1:" + ((IPEndPoint)listener.LocalEndpoint).Port;
            } finally { listener.Stop(); }
        }
        static bool LocalPortOpen(string url) {
            try {
                var uri = new Uri(url);
                using (var client = new TcpClient()) {
                    client.Connect(uri.Host, uri.Port);
                    return true;
                }
            } catch (SocketException) { return false; }
        }
        static async Task DouyinAuthorizationTests(MainForm owner, Deployment deployment, WebView2 management) {
            const string state = "111111111111111111111111111111111111111111111111";
            const string auth = "https://open.douyin.com/platform/oauth/connect?client_key=fixture-client&response_type=code&scope=user_info&redirect_uri=https%3A%2F%2Fowner.example.test%2Fcallback&state=" + state;
            const string qrPage = "https://open.douyin.com/platform/oauth/pc/auth?state=" + state;
            const string callback = "https://owner.example.test/callback?code=synthetic-code&scopes=user_info&state=" + state;
            var now = DateTimeOffset.UtcNow;
            string expires = now.AddMinutes(3).ToString("o");
            DouyinAuthorizationAttempt attempt, invalid;
            Assert(DouyinAuthorizationPolicy.TryCreate(auth, expires, now, out attempt), "official authorization URL was rejected");
            Assert(attempt.ExpiresAt == now.AddMinutes(3), "provider expiry must remain authoritative");
            Assert(!DouyinAuthorizationPolicy.TryCreate(auth, now.ToString("o"), now, out invalid), "expired attempt was accepted");
            Assert(!DouyinAuthorizationPolicy.TryCreate(auth.Replace("open.douyin.com", "open.douyin.com.example.test"), expires, now, out invalid), "foreign authority was accepted");
            Assert(!DouyinAuthorizationPolicy.TryCreate(auth + "&state=" + state, expires, now, out invalid), "duplicate state was accepted");
            Assert(attempt.AllowsNavigation(auth) && attempt.AllowsNavigation(qrPage), "official authorization navigation was rejected");
            Assert(!DouyinAuthorizationPolicy.TryCreate(qrPage, expires, now, out invalid), "QR continuation was accepted as initial URL");
            Assert(!attempt.AllowsNavigation("https://www.douyin.com/") && !attempt.AllowsNavigation("https://open.douyin.com/player/video"), "authorization must not open video or feed");
            Assert(attempt.IsCallback(callback), "matching callback was rejected");
            Assert(!attempt.IsCallback(callback + "&state=" + state) && !attempt.IsCallback(callback.Replace("/callback?", "/other?")) &&
                !attempt.IsCallback(callback.Replace(state, "old-state")), "an unrelated callback was accepted");
            await management.CoreWebView2.ExecuteScriptAsync(
                "window.fixtureAuthReply = null; chrome.webview.addEventListener('message', event => window.fixtureAuthReply = event.data);" +
                "chrome.webview.postMessage({type:'douyin-auth-open',url:'" + auth + "',expires_at:'" + now.AddMinutes(-1).ToString("o") + "'});");
            string rejected = "";
            for (int i = 0; i < 50 && rejected == ""; i++) {
                var reply = await management.CoreWebView2.ExecuteScriptAsync("window.fixtureAuthReply");
                if (reply != "null") rejected = reply;
                else await Task.Delay(100);
            }
            Assert(rejected.Contains("douyin-auth-closed") && rejected.Contains(state) && owner.OwnedForms.Length == 0,
                "desktop bridge did not reject and correlate an expired authorization request");
            const string feed = "https://www.douyin.com/";
            foreach (bool visitFeed in new[] { false, true }) {
                string result = null, closure = null;
                int requests = 0;
                bool outsideRequested = false;
                using (var dialog = new DouyinAuthorizationDialog(deployment, auth, attempt, value => result = value, value => closure = value)) {
                    var popup = dialog.Controls.OfType<WebView2>().Single();
                    popup.CoreWebView2InitializationCompleted += delegate(object sender, CoreWebView2InitializationCompletedEventArgs args) {
                        if (!args.IsSuccess) return;
                        var core = popup.CoreWebView2;
                        core.AddWebResourceRequestedFilter("*", CoreWebView2WebResourceContext.All);
                        core.WebResourceRequested += delegate(object _, CoreWebView2WebResourceRequestedEventArgs e) {
                            bool isAuth = e.Request.Uri == auth || e.Request.Uri == qrPage;
                            if (isAuth) requests++;
                            if (e.Request.Uri == callback || e.Request.Uri == feed) outsideRequested = true;
                            string content = e.Request.Uri == auth
                                ? "<script>location.replace('" + qrPage + "')</script>"
                                : "<title>Synthetic QR authorization</title><p>Synthetic QR</p>";
                            byte[] bytes = Encoding.UTF8.GetBytes("<!doctype html><link rel=icon href=data:,>" + content);
                            e.Response = core.Environment.CreateWebResourceResponse(new MemoryStream(bytes), isAuth ? 200 : 403,
                                isAuth ? "OK" : "Forbidden", "Content-Type: text/html; charset=utf-8");
                        };
                    };
                    dialog.Show(owner);
                    await Wait(() => result != null || closure != null || popup.CoreWebView2 != null &&
                        popup.CoreWebView2.DocumentTitle == "Synthetic QR authorization", "isolated authorization WebView did not render");
                    Assert(dialog.Owner == owner && dialog.Visible && !dialog.ShowInTaskbar, "authorization must be an owned application window");
                    Assert(requests == 2 && result == null && closure == null, "synthetic authorization page was not isolated and shown");
                    await popup.CoreWebView2.ExecuteScriptAsync("location.assign('" + (visitFeed ? feed : callback) + "')");
                    await Wait(() => dialog.IsDisposed, "navigation did not close the authorization window");
                    if (visitFeed)
                        Assert(result == null && closure == "授权窗口已阻止非授权页面。", "feed navigation was not rejected");
                    else
                        Assert(result == callback && closure == null, "matching callback did not complete exactly once");
                    Assert(!outsideRequested, "non-authorization navigation reached the network");
                }
            }
        }

        static void BundledAppAndToolTests(Deployment d) {
            var douyinBundled = File.Exists(Path.Combine(d.Workspace, "connected-apps", "douyin", "app.json"));
            var obsBundled = File.Exists(Path.Combine(d.Workspace, "plugins", "obs-preview", "plugin.json"));
            if (!douyinBundled && !obsBundled) return;

            using (var client = new HttpClient(new HttpClientHandler { UseProxy = false, AllowAutoRedirect = false })) {
                client.Timeout = TimeSpan.FromSeconds(5);
                const string authorization = "/api/local/apps/douyin/authorization";
                const string publications = "/api/local/publications?app_id=douyin";
                const string obsPackage = "/api/local/project-tools/obs-preview";
                const string preview = obsPackage + "/preview";
                const string live = "/api/local/live";
                if (douyinBundled) ExpectStationStatus(client, d.Url + authorization, HttpMethod.Get, null, HttpStatusCode.Unauthorized);
                ExpectStationStatus(client, d.Url + publications, HttpMethod.Get, null, HttpStatusCode.Unauthorized);
                ExpectStationStatus(client, d.Url + preview, HttpMethod.Get, null, HttpStatusCode.Unauthorized);
                ExpectStationStatus(client, d.Url + live, HttpMethod.Get, null, HttpStatusCode.Unauthorized);
                client.DefaultRequestHeaders.Add("Origin", d.Url);
                var password = Deployment.LocalStationPassword(d.Root, Environment.GetEnvironmentVariable("AHS_ADMIN_PASSWORD") ?? "");
                var login = ControllerTests.StationJSON(client, HttpMethod.Post, d.Url + "/api/login", new { password });
                client.DefaultRequestHeaders.Add("X-CSRF-Token", Deployment.Text(login, "csrf"));
                ControllerTests.StationJSON(client, HttpMethod.Post, d.Url + "/api/local/home", new { action = "initialize" });
                Func<string, Dictionary<string, object>> read = path => Deployment.Json.Deserialize<Dictionary<string, object>>(client.GetStringAsync(d.Url + path).GetAwaiter().GetResult());

                if (douyinBundled) {
                    var apps = (IList)read("/api/apps")["items"];
                    Assert(apps.Cast<Dictionary<string, object>>().Any(app => Deployment.Text(app, "id") == "douyin"),
                        "the packaged Douyin app did not register");
                    var fresh = read(authorization);
                    Assert(Deployment.Text(fresh, "status") == "unconfigured" && !Convert.ToBoolean(fresh["publish_authorized"]),
                        "bundling Douyin granted authorization before sign-in");

                    // A past consent is retained in the account record, while
                    // effective publication permission is gated by expiry.
                    const string token = "synthetic-expired-package-access";
                    var file = Path.Combine(d.Root, "apps", "douyin", "authorization.json");
                    Directory.CreateDirectory(Path.GetDirectoryName(file));
                    File.WriteAllText(file, Deployment.Json.Serialize(new { schema_version = 1, revision = new string('a', 48),
                        client_key = "synthetic-client", open_id = "synthetic-account", access_token = token, label = "Synthetic account",
                        read_works = true, publish_video = true, expires_at = DateTime.UtcNow.AddHours(-1).ToString("o") }), new UTF8Encoding(false));
                    var expired = read(authorization);
                    Assert(Deployment.Text(expired, "status") == "expired" && !Deployment.Json.Serialize(expired).Contains(token),
                        "the packaged account failed to report expiry or exposed a credential");
                    var publish = read(publications);
                    var tasks = (Dictionary<string, object>)publish["tasks"];
                    Assert(((IList)publish["targets"]).Count == 0 && ((IList)tasks["items"]).Count == 0,
                        "expired authorization exposed a publish target or fabricated a task");
                }

                if (obsBundled) {
                    ExpectStationStatus(client, d.Url + obsPackage, HttpMethod.Put, new { action = "install" }, HttpStatusCode.BadRequest);
                    ControllerTests.StationJSON(client, HttpMethod.Put, d.Url + obsPackage, new { action = "install", approve_artifacts = true });
                    ExpectStationStatus(client, d.Url + preview, HttpMethod.Post,
                        new { action = "configure", host = "192.0.2.1", port = 4455 }, HttpStatusCode.BadRequest);
                    var state = read(preview);
                    Assert(!Convert.ToBoolean(state["configured"]) && !Convert.ToBoolean(state["enabled"]) &&
                        !Convert.ToBoolean(state["active"]) && !state.ContainsKey("image_data"), "installing OBS preview must not start capture");
                }

                var liveStatus = read(live);
                Assert(!Convert.ToBoolean(liveStatus["available"]) && ((IList)liveStatus["targets"]).Count == 0 && liveStatus["active"] == null,
                    "packaged apps or OBS preview exposed an unverified live target");
                ExpectStationStatus(client, d.Url + live, HttpMethod.Post,
                    new { action = "start", app_id = "douyin", connection_id = "authorized-account", request_key = "package-live", duration_seconds = 60 },
                    HttpStatusCode.ServiceUnavailable);
            }
        }

        static void ExpectStationStatus(HttpClient client, string url, HttpMethod method, object body, HttpStatusCode expected) {
            using (var request = new HttpRequestMessage(method, url)) {
                if (body != null) request.Content = new StringContent(Deployment.Json.Serialize(body), Encoding.UTF8, "application/json");
                using (var response = client.SendAsync(request).GetAwaiter().GetResult()) {
                    Assert(response.StatusCode == expected, method + " " + new Uri(url).AbsolutePath + " returned " + (int)response.StatusCode + ", expected " + (int)expected);
                }
            }
        }

        // This intentionally uses the actual local-station executable rather
        // than SyntheticPageServer.  It proves that the desktop owns only its
        // own child, authenticates its private session and can close it again.
        static async Task LocalStationFormTests(string package, string root) {
            var d = Deployment.LocalStationDefault(Path.GetFullPath(package));
            d.Root = Path.Combine(root, "station-data");
            d.StationWorkspace = Path.Combine(d.Root, "managed-workspaces");
            d.Url = FreeLocalUrl();
            Assert(File.Exists(d.StationExe), "local station executable missing from preview package");
            // A station package can carry the optional native WeChat runtime.
            // Its presence alone must never start it or make it required.
            Assert(!d.LocalStationNative, "default local station leaves wx opt-in disabled");
            new CloseSettings(d.Workspace).Save("exit");
            using (var instance = new DesktopInstance(DesktopInstance.LocalStationName + ".window-test." + Guid.NewGuid().ToString("N")))
            using (var form = new MainForm(d, instance)) {
                Assert(instance.IsOwner, "local station window owns its isolated control name");
                form.Show();
                await Wait(() => form.Controls.OfType<WebView2>().Any(candidate => candidate.CoreWebView2 != null && candidate.Visible), "local station WebView did not initialize");
                var view = form.Controls.OfType<WebView2>().Single();
                bool localPage = false;
                for (int i = 0; i < 150 && !localPage; ++i) {
                    try {
                        var result = await view.CoreWebView2.ExecuteScriptAsync("Boolean(document.querySelector('#initialize-button, #chat-input'))");
                        localPage = result == "true";
                    } catch (InvalidOperationException) { }
                    if (!localPage) await Task.Delay(100);
                }
                Assert(localPage, "local station management page did not expose initialize or chat UI");
                BundledAppAndToolTests(d);
                await DouyinAuthorizationTests(form, d, view);
                form.Close();
                await Wait(() => form.IsDisposed, "local station exit did not close its management window");
                await Wait(() => !LocalPortOpen(d.Url), "local station exit left its owned service listening");
            }
        }
        [STAThread]
        static int Main(string[] args) {
            if (args.Length >= 2 && args[0].StartsWith("--instance-", StringComparison.Ordinal)) {
                using (var instance = new DesktopInstance(args[1])) {
                    if (args[0] == "--instance-hold") { File.WriteAllText(args[2], instance.IsOwner ? "owner" : "secondary"); Thread.Sleep(Timeout.Infinite); }
                    instance.ActivateOwner(); Console.WriteLine(instance.IsOwner ? "owner" : "secondary"); return 0;
                }
            }
            Console.OutputEncoding = new System.Text.UTF8Encoding(false);
            Program.RegisterDependencies();
            Application.EnableVisualStyles(); Application.SetCompatibleTextRenderingDefault(false);
            string localStationPackage = args.Length == 2 && args[0] == "--local-station" ? Path.GetFullPath(args[1]) : null;
            if (args.Length != 0 && localStationPackage == null) { Console.Error.WriteLine("参数：--local-station <预览包目录>"); return 2; }
            Exception failure = null;
            Application.ThreadException += delegate(object sender, ThreadExceptionEventArgs e) { failure = e.Exception; };
            string root = Path.Combine(Path.GetDirectoryName(Application.ExecutablePath), "window-tests-" + Guid.NewGuid().ToString("N")); Directory.CreateDirectory(root);
            using (var host = new Form { ShowInTaskbar = false, Opacity = 0, Location = new Point(-20000, -20000), StartPosition = FormStartPosition.Manual }) {
                host.Shown += async delegate {
                    try {
                        if (localStationPackage != null) await LocalStationFormTests(localStationPackage, root);
                        else { LoadingSurfaceTests(root); await InstanceTests(root); await FormTests(root); }
                    }
                    catch (Exception error) { failure = error; }
                    finally { host.Close(); }
                };
                Application.Run(host);
            }
            if (failure != null) { Console.Error.WriteLine(failure); return 1; }
            if (localStationPackage != null) Console.WriteLine("LocalStation desktop window assertions passed: " + assertions + "; actual isolated local service, WebView page and owned-process shutdown; no WeChat/native process.");
            else Console.WriteLine("Desktop window lifecycle assertions passed: " + assertions + "; 8 real WinForms/WebView exit cycles, 12 simultaneous duplicate probes; synthetic services, no WeChat sends.");
            return 0;
        }
    }
}
