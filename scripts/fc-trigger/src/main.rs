use std::fs::File;
use std::io::Read;
use std::os::unix::process::ExitStatusExt;
use std::process::Command;
use std::sync::atomic::{AtomicI32, Ordering};
use std::thread::sleep;
use std::time::Duration;

static CHILD_PID: AtomicI32 = AtomicI32::new(0);

extern "C" fn forward_signal(sig: libc::c_int) {
    let pid = CHILD_PID.load(Ordering::Relaxed);
    if pid > 0 {
        unsafe { libc::kill(pid, sig) };
    }
}

fn main() {
    for sig in [libc::SIGTERM, libc::SIGINT, libc::SIGHUP] {
        unsafe { libc::signal(sig, forward_signal as usize) };
    }

    loop {
        if let Ok(mut f) = File::open("/dev/vdb") {
            let mut b = [0u8; 1];
            if f.read_exact(&mut b).is_ok() && b[0] == 0x01 {
                let exit_code = match Command::new("/.fc_cmd").spawn() {
                    Ok(mut child) => {
                        CHILD_PID.store(child.id() as i32, Ordering::Relaxed);
                        let code = child
                            .wait()
                            .map(|s| s.code().unwrap_or_else(|| 128 + s.signal().unwrap_or(0)))
                            .unwrap_or(1);
                        CHILD_PID.store(0, Ordering::Relaxed);
                        code
                    }
                    Err(_) => 127,
                };

                println!("--- exit: {} ---", exit_code);
                unsafe {
                    libc::sync();
                    libc::reboot(libc::RB_AUTOBOOT);
                }
            }
        }
        sleep(Duration::from_millis(1));
    }
}
