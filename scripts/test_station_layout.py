"""Station layout and application persona replay, synthetic data and no model calls."""
import argparse
import json
import os
from pathlib import Path
import tempfile

from playwright.sync_api import sync_playwright, expect
from test_local_home_ui import start, stop, open_application, close_application, home_tab


def bounds(page):
    return page.evaluate("""() => {
      const r=document.querySelector('.home-visual').getBoundingClientRect();
      return {top:r.top,bottom:r.bottom,height:r.height,root:document.scrollingElement.scrollTop,
        documentHeight:document.documentElement.scrollHeight,viewport:innerHeight,
        overflow:document.documentElement.scrollWidth>innerWidth};
    }""")


def fixed_shell(page, expected=None):
    value=bounds(page)
    assert not value['overflow'], value
    assert value['documentHeight']<=value['viewport']+1 and value['root']==0, value
    assert value['top']>=0 and value['bottom']<=value['viewport'],value
    if expected is not None: assert abs(value['height']-expected['height'])<1, (expected,value)
    return value


def verify(binary, output):
    output.mkdir(parents=True,exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='synthetic-layout-',dir=output) as temporary:
        directory=Path(temporary).resolve()
        process,origin=start(binary,directory/'host',directory/'work')
        try:
            with sync_playwright() as playwright:
                browser=playwright.chromium.launch(channel='msedge',headless=True)
                context=browser.new_context(viewport={'width':1440,'height':900})
                page=context.new_page();errors=[]
                page.on('pageerror',lambda error:errors.append(str(error)))
                page.goto(origin)
                page.locator('#login-password').fill('synthetic-home-owner')
                with page.expect_response(lambda response:response.url.endswith('/api/login')) as login:
                    page.locator('#login-submit').click()
                csrf=login.value.json()['csrf']
                page.locator('#initialize-button').click()
                page.locator('#chat-view').wait_for(state='visible')
                initial=fixed_shell(page)
                main=context.request.get(origin+'/api/local/model').json()['persona']
                home_tab(page,'apps');page.locator('#apps-list[aria-busy="false"]').wait_for()
                assert page.get_by_role('button',name='安装接入版本',exact=True).count()==0
                page.screenshot(path=str(output/'applications-desktop.png'))
                for app,text in [('微信','合成微信偏好：喜欢音乐和简洁表达。'),('抖音','合成抖音偏好：喜欢旅行和摄影。')]:
                    open_application(page,app,'应用子角色')
                    form=page.get_by_role('form',name=app+'子角色',exact=True)
                    form.get_by_role('button',name='编辑',exact=True).click()
                    field=form.get_by_role('textbox',name=app+'子角色设定',exact=True)
                    field.fill(text)
                    form.get_by_role('button',name='保存子角色',exact=True).click()
                    expect(form.get_by_role('button',name='编辑',exact=True)).to_be_visible()
                    result=context.request.get(origin+f'/api/local/apps/{"wechat" if app=="微信" else "douyin"}/persona').json()
                    assert result['persona']==text
                    assert context.request.get(origin+'/api/local/model').json()['persona']==main
                    page.screenshot(path=str(output/f'{"wechat" if app=="微信" else "douyin"}-persona-desktop.png'))
                    close_application(page)
                # A long right-hand settings page must not resize the scene or move the document.
                home_tab(page,'project-tools');page.locator('#project-tools-list[aria-busy="false"]').wait_for()
                fixed_shell(page,initial)
                cards=page.locator('.project-tool-card')
                sizes=cards.evaluate_all('nodes => nodes.map(node => ({w:node.getBoundingClientRect().width,h:node.getBoundingClientRect().height}))')
                assert sizes and len({round(x['h']) for x in sizes})==1 and len({round(x['w']) for x in sizes})==1,sizes
                page.get_by_role('button',name='管理本机语音',exact=True).click()
                expect(page.locator('#tool-dialog')).to_be_visible()
                assert page.locator('#tool-dialog-body').evaluate('node => node.scrollWidth<=node.clientWidth+1')
                page.screenshot(path=str(output/'tool-dialog-desktop.png'))
                page.keyboard.press('Escape')
                expect(page.locator('#tool-dialog')).not_to_be_visible()
                page.locator('#project-tools-view').evaluate('node => node.scrollTop=node.scrollHeight')
                fixed_shell(page,initial)
                page.screenshot(path=str(output/'tools-desktop-scrolled.png'))
                home_tab(page,'settings')
                page.locator('#model-persona').fill('未保存的主角色草稿')
                page.locator('#settings-view').evaluate('node => node.scrollTop=node.scrollHeight')
                fixed_shell(page,initial)
                page.screenshot(path=str(output/'settings-desktop-scrolled.png'))
                home_tab(page,'apps');page.locator('#apps-list[aria-busy="false"]').wait_for()
                open_application(page,'微信','应用子角色')
                form=page.get_by_role('form',name='微信子角色',exact=True)
                form.get_by_role('button',name='编辑',exact=True).click()
                field=form.get_by_role('textbox',name='微信子角色设定',exact=True)
                field.fill('暂未保存的应用草稿')
                page.locator('#app-dialog-tabs').get_by_role('button',name='接入版本管理',exact=True).click()
                page.locator('#app-dialog-tabs').get_by_role('button',name='应用子角色',exact=True).click()
                assert field.input_value()=='暂未保存的应用草稿'
                # Force a long form body to verify the dialog scrolls and the fixed header stays usable.
                field.evaluate('node => node.style.minHeight="1000px"')
                header_top=page.locator('#app-dialog .app-dialog-header').bounding_box()['y']
                page.locator('#app-dialog-body').evaluate('node => node.scrollTop=node.scrollHeight')
                assert page.locator('#app-dialog-body').evaluate('node => node.scrollTop')>0
                assert page.locator('#app-dialog .app-dialog-header').bounding_box()['y']==header_top
                fixed_shell(page,initial)
                page.keyboard.press('Escape')
                expect(page.locator('#app-dialog')).not_to_be_visible()
                home_tab(page,'settings')
                assert page.locator('#model-persona').input_value()=='未保存的主角色草稿'
                for width,height,theme in [(1024,600,'light'),(375,740,'light'),(375,600,'dark')]:
                    close_application(page);page.set_viewport_size({'width':width,'height':height});page.emulate_media(color_scheme=theme)
                    fixed_shell(page)
                    home_tab(page,'apps');page.locator('#apps-list[aria-busy="false"]').wait_for()
                    page.screenshot(path=str(output/f'applications-{width}-{height}-{theme}.png'))
                    open_application(page,'抖音','应用子角色')
                    form=page.get_by_role('form',name='抖音子角色',exact=True)
                    form.get_by_role('button',name='编辑',exact=True).click()
                    field=form.get_by_role('textbox',name='抖音子角色设定',exact=True)
                    expect(field).to_have_value('合成抖音偏好：喜欢旅行和摄影。')
                    page.screenshot(path=str(output/f'dialog-{width}-{height}-{theme}.png'))
                    dialog=page.locator('#app-dialog').bounding_box()
                    assert dialog['x']>=0 and dialog['x']+dialog['width']<=width+1
                    assert dialog['y']>=0 and dialog['y']+dialog['height']<=height+1
                    assert not page.locator('#app-dialog-body').evaluate('node => node.scrollWidth>node.clientWidth+1')
                    form.get_by_role('button',name='取消',exact=True).click()
                    close_application(page)
                assert not errors, errors
                browser.close()
        finally:stop(process)
    print(json.dumps({'status':'passed','model_calls':0,'wechat_sends':0,'scenes':['viewport-fixed scene','independent content scrolling','large app dialog','per-app persona isolation','draft preservation','escape close','desktop and narrow light dark']},ensure_ascii=False))


if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary',required=True,type=Path)
    parser.add_argument('--output',required=True,type=Path)
    args=parser.parse_args()
    verify(args.binary.resolve(),args.output.resolve())
