use std::fs::File;
use std::io::Read;
use std::process::Command;
use std::thread::sleep;
use std::time::Duration;

fn main() {
    loop {
        if let Ok(mut file) = File::open("/dev/vdb") {
            let mut byte = [0u8; 1];
            if file.read_exact(&mut byte).is_ok() && byte[0] == 0x01 {
                let status = Command::new("/.fc_cmd")
                    .status()
                    .map(|s| s.code().unwrap_or(1))
                    .unwrap_or(1);

                println!("--- exit: {} ---", status);

                unsafe {
                    libc::sync();
                    libc::reboot(libc::RB_AUTOBOOT);
                }
            }
        }
        sleep(Duration::from_millis(1));
    }
}
