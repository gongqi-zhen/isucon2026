import jpeg from "jpeg-js";

export type JPEGImage = {
  width: number;
  height: number;
};

export function decodeJPEG(data: Uint8Array): JPEGImage | null {
  try {
    const image = jpeg.decode(data, { useTArray: true, formatAsRGBA: false, tolerantDecoding: false });
    return { width: image.width, height: image.height };
  } catch {
    return null;
  }
}
