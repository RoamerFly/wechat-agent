using System;
using System.Diagnostics;
using System.Drawing;
using System.IO;
using System.Linq;
using System.Net;
using System.Reflection;
using System.Runtime.CompilerServices;
using System.Runtime.InteropServices;
using System.Security.Cryptography;
using System.Threading;
using System.Threading.Tasks;
using System.Windows.Forms;
using Microsoft.Web.WebView2.Core;
using Microsoft.Web.WebView2.WinForms;

[assembly: AssemblyTitle("Agent-home-station 桌面版")]
[assembly: AssemblyVersion("1.1.0.0")]
namespace WechatAgent.Desktop {
    internal static class Program {
        static readonly object loaderGate = new object();
        static bool loaderConfigured;
#if LOCAL_STATION
        const bool LocalStationBuild = true;
#else
        const bool LocalStationBuild = false;
#endif
        [DllImport("shell32.dll", CharSet=CharSet.Unicode)]
        static extern int SetCurrentProcessExplicitAppUserModelID(string identity);
        [STAThread]
        static void Main(string[] args) {
            if (Array.IndexOf(args, "--prepare-legacy-cutover") >= 0) {
                Environment.ExitCode = PrepareLegacyCutover(args);
                return;
            }
            if (args.Length == 1 && (args[0] == "--start-services" || args[0] == "--stop-services" || args[0] == "--exit-services")) {
                Environment.ExitCode = DesktopInstance.RequestCommand(args[0].Substring(2), InstanceName()) ? 0 : 1;
                return;
            }
            Application.EnableVisualStyles(); Application.SetCompatibleTextRenderingDefault(false);
            try {
                // The binary family is the only authority for its control
                // namespace.  Admit before reading configuration so a second
                // instance cannot touch another instance's files or services.
                using (var instance = new DesktopInstance(InstanceName())) {
                    if (!instance.IsOwner) { instance.ActivateOwner(); return; }
                    var d = Deployment.Load(Deployment.FindWorkspace(args));
                    if (d.LocalStation != LocalStationBuild) throw new InvalidOperationException(LocalStationBuild ? "AgentHomeStation 只能打开 local-station 本机成长站配置。" : "WechatAgent 不能打开 local-station 预览配置；请使用 AgentHomeStation.exe。");
                    SetCurrentProcessExplicitAppUserModelID(LocalStationBuild ? "RoamerFly.AgentHomeStation.Local" : "RoamerFly.WechatAgent.Desktop");
                    RegisterDependencies();
                    Run(d, instance);
                }
            } catch (Exception error) { MessageBox.Show(error.Message, "Agent-home-station · 无法打开", MessageBoxButtons.OK, MessageBoxIcon.Error); }
        }
        static int PrepareLegacyCutover(string[] args) {
#if LOCAL_STATION
            Console.Error.WriteLine("请使用旧微信桌面启动器执行此交接命令。");
            return 1;
#else
            try {
                if (args.Length != 3 || args[0] != "--prepare-legacy-cutover" || args[1] != "--workspace" || (Path.GetPathRoot(args[2]) ?? "").Length != 3)
                    throw new ArgumentException("参数：--prepare-legacy-cutover --workspace <本机绝对项目目录>");
                using (var instance = new DesktopInstance(DesktopInstance.Name)) {
                    if (!instance.IsOwner) throw new InvalidOperationException("旧微信桌面仍在运行，请先正常停止。");
                    var workspace = Path.GetFullPath(args[2]);
                    WindowsRuntime.NoLinks(workspace);
                    WindowsRuntime.NoLinks(Path.Combine(workspace, "secrets"));
                    var config = Path.Combine(workspace, "secrets", "local-deployment.json");
                    if (File.Exists(config) && (File.GetAttributes(config) & FileAttributes.ReparsePoint) != 0)
                        throw new InvalidOperationException("部署配置不能使用重解析点。");
                    WindowsRuntime.PrepareOfflineCutover(Deployment.Load(workspace), CancellationToken.None).GetAwaiter().GetResult();
                    Console.WriteLine("{\"state\":\"stopped\",\"mode\":\"offline_hold\"}");
                    return 0;
                }
            } catch (Exception error) {
                Console.Error.WriteLine(error.Message);
                return 1;
            }
#endif
        }
        static string InstanceName() { return LocalStationBuild ? DesktopInstance.LocalStationName : DesktopInstance.Name; }
        internal static void RegisterDependencies() {
                AppDomain.CurrentDomain.AssemblyResolve += delegate(object sender, ResolveEventArgs e) {
                    string name = new AssemblyName(e.Name).Name;
                    if (name != "Microsoft.Web.WebView2.Core" && name != "Microsoft.Web.WebView2.WinForms") return null;
                    using (var stream = Assembly.GetExecutingAssembly().GetManifestResourceStream(name + ".dll")) {
                        var bytes = new byte[stream.Length]; stream.Read(bytes, 0, bytes.Length); return Assembly.Load(bytes);
                    }
                };
        }
        [MethodImpl(MethodImplOptions.NoInlining)]
        static void Run(Deployment d, DesktopInstance instance) { Application.Run(new MainForm(d, instance)); }
        internal static void ConfigureWebViewLoader(string workspace) {
            lock (loaderGate) {
                if (loaderConfigured) return;
                // WebView2 permits this setter only before its first API call.
                // Restarting services/pages must reuse the loaded DLL path.
                CoreWebView2Environment.SetLoaderDllFolderPath(ExtractLoader(workspace));
                loaderConfigured = true;
            }
        }
        internal static string ExtractLoader(string workspace) {
            byte[] bytes;
            using (var stream = Assembly.GetExecutingAssembly().GetManifestResourceStream("WebView2Loader.dll")) {
                bytes = new byte[stream.Length]; stream.Read(bytes, 0, bytes.Length);
            }
            string hash; using (var sha = SHA256.Create()) hash = BitConverter.ToString(sha.ComputeHash(bytes)).Replace("-", "");
            string folder = Path.Combine(workspace, "temp", "desktop", "runtime", hash);
            Directory.CreateDirectory(folder); string path = Path.Combine(folder, "WebView2Loader.dll");
            if (!File.Exists(path) || !File.ReadAllBytes(path).SequenceEqual(bytes)) File.WriteAllBytes(path, bytes);
            return folder;
        }
    }

}
