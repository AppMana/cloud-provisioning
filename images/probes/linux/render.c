// Offscreen Vulkan transfer/clear probe. This is not a Unity performance test.
#include <vulkan/vulkan.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#define CHECK(x) do { VkResult result=(x); if(result!=VK_SUCCESS){fprintf(stderr,"%s: %d\n",#x,result);exit(1);} } while(0)
static uint32_t memory(VkPhysicalDevice gpu, uint32_t bits, VkMemoryPropertyFlags flags) {
    VkPhysicalDeviceMemoryProperties props; vkGetPhysicalDeviceMemoryProperties(gpu,&props);
    for(uint32_t i=0;i<props.memoryTypeCount;i++)
        if((bits&(1u<<i)) && (props.memoryTypes[i].propertyFlags&flags)==flags) return i;
    fprintf(stderr,"Required GPU memory type unavailable\n"); exit(1);
}
int main(void) {
    VkInstance instance;
    VkApplicationInfo app={.sType=VK_STRUCTURE_TYPE_APPLICATION_INFO,.pApplicationName="cldt-offscreen",.apiVersion=VK_API_VERSION_1_1};
    VkInstanceCreateInfo ici={.sType=VK_STRUCTURE_TYPE_INSTANCE_CREATE_INFO,.pApplicationInfo=&app};
    CHECK(vkCreateInstance(&ici,NULL,&instance));
    uint32_t count=0; CHECK(vkEnumeratePhysicalDevices(instance,&count,NULL));
    VkPhysicalDevice *devices=calloc(count,sizeof(*devices)),gpu=VK_NULL_HANDLE;
    CHECK(vkEnumeratePhysicalDevices(instance,&count,devices));
    for(uint32_t i=0;i<count;i++) { VkPhysicalDeviceProperties p;vkGetPhysicalDeviceProperties(devices[i],&p);
        if(p.vendorID==0x10de && p.deviceType!=VK_PHYSICAL_DEVICE_TYPE_CPU) {gpu=devices[i];break;} }
    free(devices); if(!gpu){fprintf(stderr,"No allocated NVIDIA Vulkan GPU\n");return 1;}
    VkPhysicalDeviceProperties props;vkGetPhysicalDeviceProperties(gpu,&props);printf("Vulkan GPU: %s\n",props.deviceName);
    uint32_t queues=0;vkGetPhysicalDeviceQueueFamilyProperties(gpu,&queues,NULL);
    VkQueueFamilyProperties *families=calloc(queues,sizeof(*families));vkGetPhysicalDeviceQueueFamilyProperties(gpu,&queues,families);
    uint32_t family=0;while(family<queues && !(families[family].queueFlags&VK_QUEUE_GRAPHICS_BIT))family++;
    free(families);if(family==queues){fprintf(stderr,"No graphics queue\n");return 1;}
    float priority=1;VkDeviceQueueCreateInfo qci={.sType=VK_STRUCTURE_TYPE_DEVICE_QUEUE_CREATE_INFO,.queueFamilyIndex=family,.queueCount=1,.pQueuePriorities=&priority};
    VkDeviceCreateInfo dci={.sType=VK_STRUCTURE_TYPE_DEVICE_CREATE_INFO,.queueCreateInfoCount=1,.pQueueCreateInfos=&qci};
    VkDevice device;CHECK(vkCreateDevice(gpu,&dci,NULL,&device));VkQueue queue;vkGetDeviceQueue(device,family,0,&queue);
    VkImageCreateInfo imci={.sType=VK_STRUCTURE_TYPE_IMAGE_CREATE_INFO,.imageType=VK_IMAGE_TYPE_2D,.format=VK_FORMAT_R8G8B8A8_UNORM,.extent={64,64,1},.mipLevels=1,.arrayLayers=1,.samples=VK_SAMPLE_COUNT_1_BIT,.tiling=VK_IMAGE_TILING_OPTIMAL,.usage=VK_IMAGE_USAGE_TRANSFER_DST_BIT|VK_IMAGE_USAGE_TRANSFER_SRC_BIT,.sharingMode=VK_SHARING_MODE_EXCLUSIVE,.initialLayout=VK_IMAGE_LAYOUT_UNDEFINED};
    VkImage image;CHECK(vkCreateImage(device,&imci,NULL,&image));VkMemoryRequirements req;vkGetImageMemoryRequirements(device,image,&req);
    VkMemoryAllocateInfo alloc={.sType=VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO,.allocationSize=req.size,.memoryTypeIndex=memory(gpu,req.memoryTypeBits,VK_MEMORY_PROPERTY_DEVICE_LOCAL_BIT)};
    VkDeviceMemory imageMemory;CHECK(vkAllocateMemory(device,&alloc,NULL,&imageMemory));CHECK(vkBindImageMemory(device,image,imageMemory,0));
    VkBufferCreateInfo bci={.sType=VK_STRUCTURE_TYPE_BUFFER_CREATE_INFO,.size=64*64*4,.usage=VK_BUFFER_USAGE_TRANSFER_DST_BIT,.sharingMode=VK_SHARING_MODE_EXCLUSIVE};
    VkBuffer buffer;CHECK(vkCreateBuffer(device,&bci,NULL,&buffer));vkGetBufferMemoryRequirements(device,buffer,&req);
    alloc.allocationSize=req.size;alloc.memoryTypeIndex=memory(gpu,req.memoryTypeBits,VK_MEMORY_PROPERTY_HOST_VISIBLE_BIT|VK_MEMORY_PROPERTY_HOST_COHERENT_BIT);
    VkDeviceMemory bufferMemory;CHECK(vkAllocateMemory(device,&alloc,NULL,&bufferMemory));CHECK(vkBindBufferMemory(device,buffer,bufferMemory,0));
    VkCommandPoolCreateInfo pci={.sType=VK_STRUCTURE_TYPE_COMMAND_POOL_CREATE_INFO,.queueFamilyIndex=family};VkCommandPool pool;CHECK(vkCreateCommandPool(device,&pci,NULL,&pool));
    VkCommandBufferAllocateInfo cai={.sType=VK_STRUCTURE_TYPE_COMMAND_BUFFER_ALLOCATE_INFO,.commandPool=pool,.level=VK_COMMAND_BUFFER_LEVEL_PRIMARY,.commandBufferCount=1};
    VkCommandBuffer cmd;CHECK(vkAllocateCommandBuffers(device,&cai,&cmd));VkCommandBufferBeginInfo begin={.sType=VK_STRUCTURE_TYPE_COMMAND_BUFFER_BEGIN_INFO};CHECK(vkBeginCommandBuffer(cmd,&begin));
    VkImageSubresourceRange range={.aspectMask=VK_IMAGE_ASPECT_COLOR_BIT,.levelCount=1,.layerCount=1};
    VkImageMemoryBarrier barrier={.sType=VK_STRUCTURE_TYPE_IMAGE_MEMORY_BARRIER,.dstAccessMask=VK_ACCESS_TRANSFER_WRITE_BIT,.oldLayout=VK_IMAGE_LAYOUT_UNDEFINED,.newLayout=VK_IMAGE_LAYOUT_TRANSFER_DST_OPTIMAL,.srcQueueFamilyIndex=VK_QUEUE_FAMILY_IGNORED,.dstQueueFamilyIndex=VK_QUEUE_FAMILY_IGNORED,.image=image,.subresourceRange=range};
    vkCmdPipelineBarrier(cmd,VK_PIPELINE_STAGE_TOP_OF_PIPE_BIT,VK_PIPELINE_STAGE_TRANSFER_BIT,0,0,NULL,0,NULL,1,&barrier);
    VkClearColorValue red={.float32={1,0,0,1}};vkCmdClearColorImage(cmd,image,VK_IMAGE_LAYOUT_TRANSFER_DST_OPTIMAL,&red,1,&range);
    barrier.srcAccessMask=VK_ACCESS_TRANSFER_WRITE_BIT;barrier.dstAccessMask=VK_ACCESS_TRANSFER_READ_BIT;barrier.oldLayout=VK_IMAGE_LAYOUT_TRANSFER_DST_OPTIMAL;barrier.newLayout=VK_IMAGE_LAYOUT_TRANSFER_SRC_OPTIMAL;
    vkCmdPipelineBarrier(cmd,VK_PIPELINE_STAGE_TRANSFER_BIT,VK_PIPELINE_STAGE_TRANSFER_BIT,0,0,NULL,0,NULL,1,&barrier);
    VkBufferImageCopy copy={.imageSubresource={.aspectMask=VK_IMAGE_ASPECT_COLOR_BIT,.layerCount=1},.imageExtent={64,64,1}};vkCmdCopyImageToBuffer(cmd,image,VK_IMAGE_LAYOUT_TRANSFER_SRC_OPTIMAL,buffer,1,&copy);
    VkMemoryBarrier host={.sType=VK_STRUCTURE_TYPE_MEMORY_BARRIER,.srcAccessMask=VK_ACCESS_TRANSFER_WRITE_BIT,.dstAccessMask=VK_ACCESS_HOST_READ_BIT};
    vkCmdPipelineBarrier(cmd,VK_PIPELINE_STAGE_TRANSFER_BIT,VK_PIPELINE_STAGE_HOST_BIT,0,1,&host,0,NULL,0,NULL);
    CHECK(vkEndCommandBuffer(cmd));VkSubmitInfo submit={.sType=VK_STRUCTURE_TYPE_SUBMIT_INFO,.commandBufferCount=1,.pCommandBuffers=&cmd};CHECK(vkQueueSubmit(queue,1,&submit,VK_NULL_HANDLE));CHECK(vkQueueWaitIdle(queue));
    unsigned char *pixels;CHECK(vkMapMemory(device,bufferMemory,0,64*64*4,0,(void**)&pixels));
    for(size_t i=0;i<64*64;i++)if(memcmp(pixels+4*i,"\xff\0\0\xff",4)){fprintf(stderr,"GPU readback mismatch at pixel %zu\n",i);return 1;}
    vkUnmapMemory(device,bufferMemory);vkDestroyCommandPool(device,pool,NULL);vkDestroyBuffer(device,buffer,NULL);vkFreeMemory(device,bufferMemory,NULL);vkDestroyImage(device,image,NULL);vkFreeMemory(device,imageMemory,NULL);vkDestroyDevice(device,NULL);vkDestroyInstance(instance,NULL);
    puts("4096 offscreen pixels verified");return 0;
}
