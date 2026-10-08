// Blurred copies of a window snapshot, the backdrop of the overlays (Figma
// Screen 13). Pixel work only, so it runs on a worker thread.

use image::{imageops, RgbaImage};
use slint::{Rgba8Pixel, SharedPixelBuffer};

/// Figma background blur 32 is a gaussian of sigma 16 logical px.
const SIGMA: f32 = 16.0;
/// The blur removes all detail finer than this many physical px.
const DOWNSCALE: u32 = 4;

/// Returns the overlay backdrop. Glass cards show the same image: Figma's
/// glass over a blurred layer adds no blur of its own.
pub fn blur(shot: &SharedPixelBuffer<Rgba8Pixel>, scale_factor: f32) -> SharedPixelBuffer<Rgba8Pixel> {
    let full = RgbaImage::from_raw(shot.width(), shot.height(), shot.as_bytes().to_vec())
        .expect("snapshot buffer matches its size");
    let small = imageops::thumbnail(
        &full,
        (shot.width() / DOWNSCALE).max(1),
        (shot.height() / DOWNSCALE).max(1),
    );
    let sigma = SIGMA * scale_factor / DOWNSCALE as f32;
    let blur = imageops::fast_blur(&small, sigma);
    SharedPixelBuffer::clone_from_slice(blur.as_raw(), blur.width(), blur.height())
}
