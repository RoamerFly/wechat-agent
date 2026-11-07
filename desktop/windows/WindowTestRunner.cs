using System;
using System.ComponentModel;
using System.Diagnostics;
using System.IO;
using System.Linq;
using System.Runtime.InteropServices;

namespace WechatAgent.Desktop {
    // Run real HWND tests on a private, never-switched desktop. Off-screen
    // coordinates alone do NOT prevent Windows from adding taskbar buttons.
    internal static class WindowTestRunner {
        [StructLayout(LayoutKind.Sequential, CharSet=CharSet.Unicode)]
        struct Startup {
            internal int cb; internal string reserved, desktop, title;
            internal int x,y,width,height,xChars,yChars,fill,flags;
            internal short show,reservedBytes; internal IntPtr reservedData,input,output,error;
        }
        [StructLayout(LayoutKind.Sequential)]
        struct ProcessInfo { internal IntPtr process,thread; internal int pid,tid; }
        [DllImport("user32.dll", CharSet=CharSet.Unicode, SetLastError=true)]
        static extern IntPtr CreateDesktop(string name, IntPtr device, IntPtr mode, int flags, uint access, IntPtr security);
        [DllImport("user32.dll")] static extern bool CloseDesktop(IntPtr desktop);
        [DllImport("kernel32.dll", CharSet=CharSet.Unicode, SetLastError=true)]
        static extern bool CreateProcess(string app, System.Text.StringBuilder command, IntPtr processSecurity, IntPtr threadSecurity, bool inherit, uint flags, IntPtr environment, string directory, ref Startup startup, out ProcessInfo process);
        [DllImport("kernel32.dll")] static extern IntPtr GetStdHandle(int kind);
        [DllImport("kernel32.dll")] static extern uint ResumeThread(IntPtr thread);
        [DllImport("kernel32.dll")] static extern bool CloseHandle(IntPtr handle);
        [DllImport("kernel32.dll")] static extern bool TerminateProcess(IntPtr process,uint code);
        delegate bool WindowCallback(IntPtr window,IntPtr argument);
        [DllImport("user32.dll")] static extern bool EnumWindows(WindowCallback callback,IntPtr argument);
        [DllImport("user32.dll")] static extern uint GetWindowThreadProcessId(IntPtr window,out uint process);
        static int Main(string[] args) {
            IntPtr desktop=IntPtr.Zero; ProcessInfo info=new ProcessInfo();
            try {
                if(args.Length<1)throw new ArgumentException("Expected a window-test executable");
                string path=Path.GetFullPath(args[0]), name="WechatAgentTests-"+Guid.NewGuid().ToString("N");
                // On the hosted GitHub runner, WebView2 initialization failed
                // with Invalid window handle (0x80070578) on the secondary
                // CreateDesktop desktop. Do not weaken the page assertion: run
                // the same executable on the runner's default desktop there.
                // Restrict this exception to GitHub Actions so local verification
                // remains a private-desktop test and never creates a taskbar
                // button for the user.
                bool githubActions=string.Equals(Environment.GetEnvironmentVariable("GITHUB_ACTIONS"),"true",StringComparison.OrdinalIgnoreCase)
                    && string.Equals(Environment.GetEnvironmentVariable("CI"),"true",StringComparison.OrdinalIgnoreCase);
                if(!githubActions) {
                    desktop=CreateDesktop(name,IntPtr.Zero,IntPtr.Zero,0,0x01ff,IntPtr.Zero);
                    if(desktop==IntPtr.Zero)throw new Win32Exception();
                }
                var startup=new Startup { cb=Marshal.SizeOf(typeof(Startup)), desktop=githubActions ? null : "winsta0\\"+name, flags=0x100, input=GetStdHandle(-10), output=GetStdHandle(-11), error=GetStdHandle(-12) };
                string command=String.Join(" ",args.Select(HiddenRunner.Quote));
                if(!CreateProcess(path,new System.Text.StringBuilder(command),IntPtr.Zero,IntPtr.Zero,true,0x08000004,IntPtr.Zero,Path.GetDirectoryName(path),ref startup,out info))throw new Win32Exception();
                using(var child=Process.GetProcessById(info.pid))using(var job=new ProcessJob()) {
                    job.Assign(child); ResumeThread(info.thread);
                    var deadline=DateTime.UtcNow.AddMinutes(2);
                    while(!child.WaitForExit(100)) {
                        if(!githubActions) {
                            bool leaked=false;
                            EnumWindows(delegate(IntPtr window,IntPtr argument){uint pid;GetWindowThreadProcessId(window,out pid);if(pid==info.pid)leaked=true;return true;},IntPtr.Zero);
                            if(leaked)throw new InvalidOperationException("A test HWND leaked onto the user's desktop");
                        }
                        if(DateTime.UtcNow>deadline)throw new TimeoutException("Isolated window tests did not finish");
                    }
                    Console.WriteLine(githubActions
                        ? "Window tests used the GitHub Actions default desktop after the hosted runner's private CreateDesktop run failed to initialize WebView2 (Invalid window handle); page and lifecycle assertions remained enabled."
                        : "Window tests used a private desktop; no test windows on the user's taskbar.");
                    return child.ExitCode;
                }
            } catch(Exception error) { if(info.process!=IntPtr.Zero)TerminateProcess(info.process,1);Console.Error.WriteLine(error); return 1; }
            finally { if(info.thread!=IntPtr.Zero)CloseHandle(info.thread);if(info.process!=IntPtr.Zero)CloseHandle(info.process);if(desktop!=IntPtr.Zero)CloseDesktop(desktop); }
        }
    }
}
