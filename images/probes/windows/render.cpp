// Hardware-only Direct3D 11 rasterization with readback, followed by raw frames
// for a separate NVENC encode. Run inside an allocated application container.
#include <windows.h>
#include <d3d11.h>
#include <d3dcompiler.h>
#include <dxgi.h>
#include <cstdio>
#include <cstring>
#include <stdexcept>
#include <string>

template<class T> struct Com {
    T* p = nullptr;
    ~Com() { if (p) p->Release(); }
    T** out() { return &p; }
    T* operator->() const { return p; }
};
static void check(HRESULT result, const char* operation) {
    if (FAILED(result)) {
        char message[160];
        std::snprintf(message, sizeof(message), "%s failed: 0x%08lx", operation, (unsigned long)result);
        throw std::runtime_error(message);
    }
}
int main(int argc, char** argv) {
    try {
        if (argc != 2) throw std::runtime_error("usage: render.exe OUTPUT.bgra");
        Com<ID3D11Device> device;
        Com<ID3D11DeviceContext> context;
        D3D_FEATURE_LEVEL level;
        const D3D_FEATURE_LEVEL levels[] = {D3D_FEATURE_LEVEL_11_0};
        check(D3D11CreateDevice(nullptr, D3D_DRIVER_TYPE_HARDWARE, nullptr, 0,
              levels, 1, D3D11_SDK_VERSION, device.out(), &level, context.out()), "hardware device");
        Com<IDXGIDevice> dxgi;
        check(device->QueryInterface(__uuidof(IDXGIDevice), (void**)dxgi.out()), "DXGI device");
        Com<IDXGIAdapter> adapter;
        check(dxgi->GetAdapter(adapter.out()), "DXGI adapter");
        DXGI_ADAPTER_DESC description{};
        check(adapter->GetDesc(&description), "adapter description");
        if (description.VendorId != 0x10de) throw std::runtime_error("expected allocated NVIDIA hardware");
        const char* shader =
            "float4 vs(uint id : SV_VertexID) : SV_POSITION {"
            "float2 v[3]={float2(-0.75,-0.75),float2(0,0.75),float2(0.75,-0.75)};"
            "return float4(v[id],0,1);}"
            "float4 ps() : SV_TARGET { return float4(0,1,0,1); }";
        Com<ID3DBlob> vsCode, psCode;
        check(D3DCompile(shader, std::strlen(shader), nullptr, nullptr, nullptr,
              "vs", "vs_5_0", 0, 0, vsCode.out(), nullptr), "vertex shader compile");
        check(D3DCompile(shader, std::strlen(shader), nullptr, nullptr, nullptr,
              "ps", "ps_5_0", 0, 0, psCode.out(), nullptr), "pixel shader compile");
        Com<ID3D11VertexShader> vs;
        Com<ID3D11PixelShader> ps;
        check(device->CreateVertexShader(vsCode->GetBufferPointer(), vsCode->GetBufferSize(), nullptr, vs.out()), "vertex shader");
        check(device->CreatePixelShader(psCode->GetBufferPointer(), psCode->GetBufferSize(), nullptr, ps.out()), "pixel shader");
        D3D11_TEXTURE2D_DESC texture{};
        texture.Width=64; texture.Height=64; texture.MipLevels=1; texture.ArraySize=1;
        texture.Format=DXGI_FORMAT_B8G8R8A8_UNORM; texture.SampleDesc.Count=1;
        texture.Usage=D3D11_USAGE_DEFAULT; texture.BindFlags=D3D11_BIND_RENDER_TARGET;
        Com<ID3D11Texture2D> target, staging;
        check(device->CreateTexture2D(&texture, nullptr, target.out()), "render target");
        texture.Usage=D3D11_USAGE_STAGING; texture.BindFlags=0; texture.CPUAccessFlags=D3D11_CPU_ACCESS_READ;
        check(device->CreateTexture2D(&texture, nullptr, staging.out()), "readback texture");
        Com<ID3D11RenderTargetView> view;
        check(device->CreateRenderTargetView(target.p, nullptr, view.out()), "target view");
        D3D11_RASTERIZER_DESC raster{};
        raster.FillMode=D3D11_FILL_SOLID; raster.CullMode=D3D11_CULL_NONE; raster.DepthClipEnable=TRUE;
        Com<ID3D11RasterizerState> state;
        check(device->CreateRasterizerState(&raster, state.out()), "rasterizer");
        context->RSSetState(state.p);
        D3D11_VIEWPORT viewport{0,0,64,64,0,1}; context->RSSetViewports(1,&viewport);
        context->OMSetRenderTargets(1,&view.p,nullptr);
        context->VSSetShader(vs.p,nullptr,0); context->PSSetShader(ps.p,nullptr,0);
        context->IASetPrimitiveTopology(D3D11_PRIMITIVE_TOPOLOGY_TRIANGLELIST);
        FILE* output=std::fopen(argv[1],"wb");
        if (!output) throw std::runtime_error("cannot open raw frame output");
        unsigned green=0, blue=0;
        for (int frame=0; frame<30; ++frame) {
            const float clear[]={0,0,1,1}; context->ClearRenderTargetView(view.p,clear);
            context->Draw(3,0); context->CopyResource(staging.p,target.p);
            D3D11_MAPPED_SUBRESOURCE map{};
            check(context->Map(staging.p,0,D3D11_MAP_READ,0,&map), "GPU readback");
            green=0; blue=0; bool written=true;
            for (unsigned y=0; y<64; ++y) {
                const auto* row=(const unsigned char*)map.pData+y*map.RowPitch;
                for (unsigned x=0;x<64;++x) {
                    const auto* pixel=row+x*4;
                    if (pixel[0]==0 && pixel[1]==255 && pixel[2]==0 && pixel[3]==255) ++green;
                    if (pixel[0]==255 && pixel[1]==0 && pixel[2]==0 && pixel[3]==255) ++blue;
                }
                written &= std::fwrite(row,1,64*4,output)==64*4;
            }
            context->Unmap(staging.p,0);
            if (!written || green<200 || blue<200 || green+blue!=4096) {
                std::fclose(output); throw std::runtime_error("rasterized pixels or frame output failed validation");
            }
        }
        if (std::fclose(output)!=0) throw std::runtime_error("raw frame flush failed");
        std::printf("{\"api\":\"Direct3D11\",\"vendorID\":4318,\"deviceID\":%u,\"frames\":30,\"pixelsPerFrame\":4096,\"greenPixels\":%u,\"bluePixels\":%u,\"hardwareOnly\":true}\n",description.DeviceId,green,blue);
        return 0;
    } catch (const std::exception& error) {
        std::fprintf(stderr,"%s\n",error.what()); return 1;
    }
}
